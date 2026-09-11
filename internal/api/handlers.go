package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/indexer"
	"github.com/Skanislav/evm-scan/internal/price"
	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/token"
)

// --------------------------------------------------------------------------
// Assets
// --------------------------------------------------------------------------

type assetJSON struct {
	ChainID       uint64 `json:"chain_id"`
	Address       string `json:"address"`
	Standard      string `json:"standard"`
	Symbol        string `json:"symbol,omitempty"`
	Name          string `json:"name,omitempty"`
	Decimals      *int16 `json:"decimals,omitempty"`
	Status        string `json:"status"`
	Source        string `json:"source"`
	Registrant    string `json:"registrant,omitempty"`
	HintFromBlock uint64 `json:"hint_from_block"`
	AnchorBlock   uint64 `json:"anchor_block,omitempty"`
	BackfillNext  uint64 `json:"backfill_next,omitempty"`
	BackfillDone  bool   `json:"backfill_done"`
	TailBlock     uint64 `json:"tail_block,omitempty"`
	LogsSeen      uint64 `json:"logs_seen,omitempty"`
	// BackfillFloor is where the history walk stops. When it is above the requested
	// from_block the node no longer holds the rest, and HistoryComplete says so
	// rather than letting the response imply full coverage.
	BackfillFloor   uint64 `json:"backfill_floor"`
	HistoryComplete bool   `json:"history_complete"`
	Promoted        bool   `json:"promoted_from_discovery,omitempty"`
	// FundingWei is what the registry still holds to pay for indexing this asset;
	// PaidFrom/PaidTo is the block range publishers have already been paid for.
	// Absent when no registry is configured.
	FundingWei string `json:"funding_wei,omitempty"`
	PaidFrom   uint64 `json:"paid_from,omitempty"`
	PaidTo     uint64 `json:"paid_to,omitempty"`
	// VouchedWei is every wei ever committed to this asset, which never goes down.
	// FundingWei drains as publishers claim coverage, so a well-indexed asset that
	// someone paid a lot for reads as zero there — the same as one nobody wanted.
	// This is the number to rank a list of contracts by.
	VouchedWei string `json:"vouched_wei,omitempty"`
	// Reports is how many complaints this asset has. One is enough to sink it: it
	// orders a scan and decides nothing else, and the two mistakes available here
	// are not the same size. Absent when nobody has complained.
	Reports      int    `json:"reports,omitempty"`
	ReportReason string `json:"report_reason,omitempty"`
}

func (s *Server) assetView(ctx context.Context, a store.Asset) assetJSON {
	v := assetJSON{
		ChainID:       a.ChainID,
		Address:       a.Address.Hex(),
		Standard:      standardName(a.Standard),
		Symbol:        a.Symbol,
		Name:          a.Name,
		Decimals:      a.Decimals,
		Status:        a.Status,
		Source:        a.Source,
		HintFromBlock: a.HintFromBlock,
		Promoted:      a.Promoted,
		Reports:       a.Reports,
		ReportReason:  a.ReportReason,
	}
	if a.Registrant != nil {
		v.Registrant = a.Registrant.Hex()
	}
	if c, err := s.d.Store.GetCursor(ctx, a.ChainID, a.Address); err == nil {
		v.AnchorBlock = c.AnchorBlock
		v.BackfillNext = c.BackfillNext
		v.BackfillDone = c.BackfillDone
		v.TailBlock = c.TailBlock
		v.LogsSeen = c.LogsSeen
		v.BackfillFloor = c.BackfillFloor
		// Complete means the walk reached as far back as was asked for, not that it
		// reached genesis: genesis may simply not be available on this node.
		v.HistoryComplete = c.BackfillDone && c.BackfillFloor <= a.HintFromBlock
	}
	if s.d.Registry != nil {
		if f, err := s.d.Registry.Funding(ctx, hintreg.AssetKey(a.ChainID, a.Address)); err == nil {
			v.FundingWei = f.Balance.String()
			if f.Vouched != nil {
				v.VouchedWei = f.Vouched.String()
			}
			v.PaidFrom, v.PaidTo = f.PaidFrom, f.PaidTo
		}
	}
	return v
}

func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}

	assets, err := s.d.Store.ListAssets(r.Context(), chainID, r.URL.Query().Get("include_revoked") == "true")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	out := make([]assetJSON, 0, len(assets))
	for _, a := range assets {
		out = append(out, s.assetView(r.Context(), a))
	}
	orderAssets(out)
	writeJSON(w, http.StatusOK, map[string]any{"chain_id": chainID, "assets": out})
}

// orderAssets is the scan order: what a reader should ask about first.
//
// Two signals, and one of them is not a tiebreak. A report sends a contract to the
// bottom regardless of what was paid for it, because the alternative is that buying
// the most funding buys the top of somebody's wallet — and the whole point of an open
// registry is that anyone can pay into it, including whoever minted the scam. Paying
// for a scam is still welcome, in the sense that the gas subsidises the honest
// assets; it simply does not come with a position.
//
// Under that, more wei vouched ranks higher. Not because money is a proof of quality,
// but because it is the only signal here that costs the person producing it — and
// ordering, unlike indexing, has to be decided for contracts nobody has judged yet.
//
// The sort is stable, so the store's chain and address ordering survives as the last
// tiebreak and a list with no funding and no reports stays in a fixed order rather
// than shuffling per request.
func orderAssets(v []assetJSON) {
	sort.SliceStable(v, func(i, j int) bool {
		if (v[i].Reports > 0) != (v[j].Reports > 0) {
			return v[j].Reports > 0
		}
		a, b := weiOrZero(v[i].VouchedWei), weiOrZero(v[j].VouchedWei)
		return a.Cmp(b) > 0
	})
}

// weiOrZero parses a decimal wei string, treating anything unparseable as nothing
// vouched. A registry that could not be read leaves the field empty, and an asset
// nobody can price should sort as if nobody paid rather than as if everybody did.
func weiOrZero(s string) *big.Int {
	if s == "" {
		return new(big.Int)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

func (s *Server) getAsset(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	addr, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}

	a, err := s.d.Store.GetAsset(r.Context(), chainID, addr)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "asset not registered", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	writeJSON(w, http.StatusOK, s.assetView(r.Context(), a))
}

type registerRequest struct {
	ChainID   uint64 `json:"chain_id"`
	Address   string `json:"address"`
	FromBlock uint64 `json:"from_block"`
}

// registerAsset adds a hint directly, without going through the on-chain registry.
//
// This is the local convenience path. It is gated behind api.allow_registration
// because the on-chain HintRegistry is the permissionless, bonded, publicly auditable
// way in; this one has no bond and no public record.
func (s *Server) registerAsset(w http.ResponseWriter, r *http.Request) {
	if !s.d.AllowRegistration {
		writeErr(w, http.StatusForbidden, "direct registration disabled",
			errors.New("register through the on-chain HintRegistry, or set api.allow_registration"))
		return
	}

	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body", err)
		return
	}

	chainID := req.ChainID
	if chainID == 0 && s.d.Chains.Len() == 1 {
		if e, ok := s.d.Chains.First(); ok {
			chainID = e.ID
		}
	}
	src, ok := s.d.Chains.Source(chainID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown chain", nil)
		return
	}
	addr, err := parseAddress(req.Address)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}

	ctx := r.Context()
	head, err := src.HeadBlock(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "node unreachable", err)
		return
	}

	// Refuse an address with no code: a hint for an EOA can only ever waste scans.
	code, err := src.CodeAt(ctx, addr)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "node unreachable", err)
		return
	}
	if len(code) == 0 {
		writeErr(w, http.StatusBadRequest, "address has no contract code", nil)
		return
	}

	from := req.FromBlock
	if from > head {
		from = 0
	}

	meta := token.Probe(ctx, src, addr)
	created, err := s.d.Store.RegisterAsset(ctx, store.Asset{
		ChainID:       chainID,
		Address:       addr,
		HintFromBlock: from,
		Symbol:        meta.Symbol,
		Name:          meta.Name,
		Decimals:      meta.Decimals,
		Source:        store.SourceLocal,
	}, head)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "registration failed", err)
		return
	}
	if wk, ok := s.d.Chains.Worker(chainID); ok {
		wk.Nudge()
	}

	a, err := s.d.Store.GetAsset(ctx, chainID, addr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	code2 := http.StatusOK
	if created {
		code2 = http.StatusCreated
	}
	writeJSON(w, code2, map[string]any{"created": created, "asset": s.assetView(ctx, a)})
}

func (s *Server) assetAccounts(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	addr, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	limit := intParam(r, "limit", 100, 1, 1000)
	offset := intParam(r, "offset", 0, 0, 1_000_000)

	rows, err := s.d.Store.AssetHolders(r.Context(), chainID, addr, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	type holder struct {
		Account    string   `json:"account"`
		FirstBlock uint64   `json:"first_block"`
		LastBlock  uint64   `json:"last_block"`
		EventCount uint64   `json:"event_count"`
		Roles      []string `json:"roles"`
	}
	out := make([]holder, 0, len(rows))
	for _, r0 := range rows {
		out = append(out, holder{
			Account:    r0.Account.Hex(),
			FirstBlock: r0.FirstBlock,
			LastBlock:  r0.LastBlock,
			EventCount: r0.EventCount,
			Roles:      indexer.RoleNames(r0.Roles),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id": chainID, "asset": addr.Hex(), "accounts": out,
	})
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo {
		return def
	}
	if v > hi {
		return hi
	}
	return v
}

// --------------------------------------------------------------------------
// Accounts: the discovery surface
// --------------------------------------------------------------------------

type accountAssetJSON struct {
	Address      string   `json:"address"`
	Standard     string   `json:"standard"`
	Symbol       string   `json:"symbol,omitempty"`
	Name         string   `json:"name,omitempty"`
	Decimals     *int16   `json:"decimals,omitempty"`
	FirstBlock   uint64   `json:"first_block"`
	LastBlock    uint64   `json:"last_block"`
	EventCount   uint64   `json:"event_count"`
	Roles        []string `json:"roles"`
	IndexStatus  string   `json:"index_status"`
	Balance      *string  `json:"balance,omitempty"`
	BalanceError string   `json:"balance_error,omitempty"`
	// Price and ValueUSD come from the chain's own oracles and pools, when the
	// deployment has pricing configured and a source was found. Never zero.
	Price    *quoteJSON `json:"price,omitempty"`
	ValueUSD string     `json:"value_usd,omitempty"`
}

// accountAssets is the rich view: discovery hints plus live balances read from our
// own node at head.
func (s *Server) accountAssets(w http.ResponseWriter, r *http.Request) {
	chainID, src, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}

	ctx := r.Context()
	rows, err := s.d.Store.AccountAssets(ctx, chainID, account)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	out := make([]accountAssetJSON, len(rows))
	for i, a := range rows {
		out[i] = accountAssetJSON{
			Address:     a.Asset.Hex(),
			Standard:    standardName(a.Standard),
			Symbol:      a.Symbol,
			Name:        a.Name,
			Decimals:    a.Decimals,
			FirstBlock:  a.FirstBlock,
			LastBlock:   a.LastBlock,
			EventCount:  a.EventCount,
			Roles:       indexer.RoleNames(a.Roles),
			IndexStatus: a.Status,
		}
	}

	// The lens reports the block it ran at, so when balances came back there is no
	// need to ask for the head separately — and no window in which the two disagree.
	var head uint64
	if r.URL.Query().Get("balances") != "false" {
		// Only when the lens read the whole list in one go: a stitched read has no
		// single block to be as of, and saying otherwise would be a small lie.
		if block, atomic := s.fillBalances(ctx, src, account, rows, out); atomic {
			head = block
		}
	}
	if head == 0 {
		head, _ = src.HeadBlock(ctx)
	}

	resp := map[string]any{
		"account":     account.Hex(),
		"chain_id":    chainID,
		"as_of_block": head,
		"assets":      out,
	}
	if h := s.hintName(account, chainID); h != "" {
		resp["hint_name"] = h
	}
	if p := s.pricerFor(chainID); p != nil && r.URL.Query().Get("prices") != "false" {
		resp["valuation"] = s.valueAssets(ctx, p, rows, out)
	}
	writeJSON(w, http.StatusOK, resp)
}

// valueAssets prices the fungible assets in an account view and sums them.
//
// NFTs are skipped: a floor price is a market question, not an oracle one, and a
// number for it here would be a guess dressed as a read.
func (s *Server) valueAssets(ctx context.Context, p *price.Pricer, rows []store.AccountAsset, out []accountAssetJSON) map[string]any {
	var addrs []common.Address
	for i := range rows {
		if out[i].Standard == "erc20" && out[i].Balance != nil {
			addrs = append(addrs, rows[i].Asset)
		}
	}
	tot := newTotals()
	if len(addrs) == 0 {
		return tot.view(0, "")
	}
	res, errMsg := s.quotes(ctx, p, addrs)
	if res == nil {
		return tot.view(0, errMsg)
	}
	for i := range rows {
		q := res.Quotes[rows[i].Asset]
		if q == nil || out[i].Balance == nil {
			continue
		}
		view := quoteView(q)
		out[i].Price = &view
		bal, ok := new(big.Int).SetString(*out[i].Balance, 10)
		if !ok {
			continue
		}
		v := valuation(bal, rows[i].Decimals, q)
		out[i].ValueUSD = price.FormatValue(v)
		tot.add(v, q)
	}
	return tot.view(res.AsOfBlock, errMsg)
}

// accountContracts is the minimal wallet-facing surface: just the contract list.
//
// This is the endpoint the whole project exists to serve. A wallet or discovery
// service asks "what should I pull history for", gets a short list instead of
// scanning the chain, and then fetches the actual history itself from any source it
// trusts. Nothing here has to be believed.
func (s *Server) accountContracts(w http.ResponseWriter, r *http.Request) {
	chainID, src, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}

	ctx := r.Context()
	rows, err := s.d.Store.AccountAssets(ctx, chainID, account)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	type hint struct {
		Address    string `json:"address"`
		Standard   string `json:"standard"`
		FirstBlock uint64 `json:"first_block"`
		LastBlock  uint64 `json:"last_block"`
	}
	hints := make([]hint, len(rows))
	for i, a := range rows {
		hints[i] = hint{
			Address:    a.Asset.Hex(),
			Standard:   standardName(a.Standard),
			FirstBlock: a.FirstBlock,
			LastBlock:  a.LastBlock,
		}
	}

	head, _ := src.HeadBlock(ctx)
	resp := map[string]any{
		"account":     account.Hex(),
		"chain_id":    chainID,
		"as_of_block": head,
		"contracts":   hints,
		"disclaimer":  "discovery hint over registered assets only; verify against the chain",
	}
	if h := s.hintName(account, chainID); h != "" {
		resp["hint_name"] = h
	}
	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------------
// Commitments
// --------------------------------------------------------------------------

type epochJSON struct {
	ID         int64  `json:"id"`
	ChainID    uint64 `json:"chain_id"`
	FromBlock  uint64 `json:"from_block"`
	ToBlock    uint64 `json:"to_block"`
	MerkleRoot string `json:"merkle_root"`
	// CoverageRoot commits to the per-asset block ranges the publisher stands
	// behind; it is what the registry pays against.
	CoverageRoot string `json:"coverage_root,omitempty"`
	LeafCount    int64  `json:"leaf_count"`
	URI          string `json:"uri,omitempty"`
	OnchainID    *int64 `json:"onchain_epoch_id,omitempty"`
	TxHash       string `json:"tx_hash,omitempty"`
	Status       string `json:"status"`
	// ExpectedRewardWei is what the registry quoted for the coverage when the epoch
	// was built; RewardWei and ClaimTx are what was actually paid after finalization.
	ExpectedRewardWei string `json:"expected_reward_wei,omitempty"`
	RewardWei         string `json:"reward_wei,omitempty"`
	ClaimTx           string `json:"claim_tx,omitempty"`
	// FilterKeccak is the digest of the .xorf membership filter over the same index
	// at the same block. Absent for epochs built before migration 0008.
	FilterKeccak string `json:"filter_keccak,omitempty"`
	// SubmissionRef is what the submitter handed back when the commitment was sent,
	// present from the moment it left this process.
	SubmissionRef string `json:"submission_ref,omitempty"`
	// OnchainStatus and ChallengeDeadline come from the registry itself, so a
	// consumer of a proposed commitment can see that it is not final yet.
	OnchainStatus     string `json:"onchain_status,omitempty"`
	ChallengeDeadline uint64 `json:"challenge_deadline,omitempty"`
	// Coverage lists the per-asset ranges behind CoverageRoot. Only on GET /v1/epochs/{id}.
	Coverage []coverageJSON `json:"coverage,omitempty"`
}

type coverageJSON struct {
	Asset     string `json:"asset"`
	Key       string `json:"key"`
	FromBlock uint64 `json:"from_block"`
	ToBlock   uint64 `json:"to_block"`
}

func epochView(e store.Epoch) epochJSON {
	v := epochJSON{
		ID: e.ID, ChainID: e.ChainID, FromBlock: e.FromBlock, ToBlock: e.ToBlock,
		MerkleRoot: e.MerkleRoot.Hex(), LeafCount: e.LeafCount, URI: e.URI,
		OnchainID: e.OnchainID, Status: e.Status,
		ExpectedRewardWei: e.ExpectedRewardWei, RewardWei: e.RewardWei,
	}
	if e.FilterKeccak != (common.Hash{}) {
		v.FilterKeccak = e.FilterKeccak.Hex()
	}
	if e.CoverageRoot != (common.Hash{}) {
		v.CoverageRoot = e.CoverageRoot.Hex()
	}
	if e.TxHash != nil {
		v.TxHash = e.TxHash.Hex()
	}
	if e.SubmissionRef != nil {
		v.SubmissionRef = e.SubmissionRef.Hex()
	}
	if e.ClaimTx != nil {
		v.ClaimTx = e.ClaimTx.Hex()
	}
	return v
}

// withOnchain adds the registry's view of a published commitment. Best effort: an
// unreachable registry leaves the fields empty rather than failing the request.
func (s *Server) withOnchain(ctx context.Context, v epochJSON) epochJSON {
	if s.d.Registry == nil || v.OnchainID == nil {
		return v
	}
	if e, err := s.d.Registry.GetEpoch(ctx, *v.OnchainID); err == nil {
		v.OnchainStatus = e.Status.String()
		v.ChallengeDeadline = e.ChallengeDeadline
	}
	return v
}

func (s *Server) listEpochs(w http.ResponseWriter, r *http.Request) {
	chainID := uint64(0)
	if raw := r.URL.Query().Get("chain_id"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad chain_id", err)
			return
		}
		chainID = v
	}

	rows, err := s.d.Store.ListEpochs(r.Context(), chainID, intParam(r, "limit", 20, 1, 200))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	// The registry's view rides along for epochs whose window may still be open, so
	// a reader can see when a commitment stops being challengeable without asking
	// for each one by id. Finalized rows are settled and need no eth_call; the
	// whole pass shares one short deadline so a slow registry costs the list a
	// few fields, never the response.
	onctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out := make([]epochJSON, 0, len(rows))
	for _, e := range rows {
		v := epochView(e)
		if e.OnchainID != nil && e.Status != store.EpochFinalized && e.Status != store.EpochRejected && onctx.Err() == nil {
			v = s.withOnchain(onctx, v)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"epochs": out})
}

type createEpochRequest struct {
	ChainID uint64 `json:"chain_id"`
	URI     string `json:"uri"`
	Publish bool   `json:"publish"`
	// Force builds even when the root matches the last commitment.
	Force bool `json:"force"`
}

// createEpoch builds a commitment and optionally posts it on-chain.
func (s *Server) createEpoch(w http.ResponseWriter, r *http.Request) {
	if s.d.Publisher == nil {
		writeErr(w, http.StatusServiceUnavailable, "publisher not configured",
			errors.New("set registry.address and registry.publisher_key"))
		return
	}

	var req createEpochRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad request body", err)
			return
		}
	}
	chainID := req.ChainID
	if chainID == 0 && s.d.Chains.Len() == 1 {
		if e, ok := s.d.Chains.First(); ok {
			chainID = e.ID
		}
	}
	if _, ok := s.d.Chains.Source(chainID); !ok {
		writeErr(w, http.StatusBadRequest, "unknown chain", nil)
		return
	}

	ctx := r.Context()
	e, err := s.d.Publisher.Build(ctx, chainID, req.URI, req.Force)
	if errors.Is(err, hintreg.ErrEmptyIndex) {
		writeErr(w, http.StatusConflict, "index is empty", err)
		return
	}
	if errors.Is(err, hintreg.ErrUnchanged) {
		writeErr(w, http.StatusConflict, "index and coverage unchanged since last commitment (set force to build anyway)", err)
		return
	}
	if errors.Is(err, hintreg.ErrUnfunded) {
		writeErr(w, http.StatusPaymentRequired, "coverage is worth less than the publisher's minimum; fund the assets or lower min_expected_reward_wei", err)
		return
	}
	if errors.Is(err, hintreg.ErrUntrusted) {
		writeErr(w, http.StatusForbidden,
			fmt.Sprintf("chain %d is not verified, so its data may not back a bonded commitment", chainID),
			fmt.Errorf("%w — its logs came from a node this deployment does not run; "+
				`promote it deliberately with PATCH /v1/chains/%d {"trust":"verified"} if you `+
				"are willing to stake the publisher's bond on that endpoint", err, chainID))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "build failed", err)
		return
	}

	if !req.Publish {
		writeJSON(w, http.StatusCreated, epochView(e))
		return
	}

	if _, err := s.d.Publisher.Publish(ctx, e.ID); err != nil {
		writeErr(w, http.StatusBadGateway, "publish failed", err)
		return
	}
	stored, err := s.d.Store.GetEpoch(ctx, e.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, s.withOnchain(ctx, epochView(stored)))
}

func (s *Server) getEpoch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad epoch id", err)
		return
	}
	e, err := s.d.Store.GetEpoch(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "epoch not found", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	v := epochView(e)
	if cov, err := s.d.Store.EpochCoverage(r.Context(), id); err == nil {
		for _, c := range cov {
			v.Coverage = append(v.Coverage, coverageJSON{
				Asset: c.Asset.Hex(), Key: c.RegistryKey.Hex(), FromBlock: c.FromBlock, ToBlock: c.ToBlock,
			})
		}
	}
	writeJSON(w, http.StatusOK, s.withOnchain(r.Context(), v))
}

// epochProof returns everything needed to call HintRegistry.verifyInclusion.
func (s *Server) epochProof(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad epoch id", err)
		return
	}
	account, err := parseAddress(r.URL.Query().Get("account"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad account", err)
		return
	}

	ctx := r.Context()
	e, err := s.d.Store.GetEpoch(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "epoch not found", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	pr, err := s.accountProof(ctx, e, account)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "account not in this commitment", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "proof failed", err)
		return
	}

	hexProof := make([]string, len(pr.proof))
	for i, p := range pr.proof {
		hexProof[i] = p.Hex()
	}
	// The asset list is echoed so a verifier can recompute assets_hash independently
	// rather than taking our digest on trust.
	addrs := make([]string, 0, len(pr.assets))
	for _, a := range pr.assets {
		addrs = append(addrs, a.Hex())
	}
	leaf := pr.leaf

	writeJSON(w, http.StatusOK, map[string]any{
		"epoch":            epochView(e),
		"account":          account.Hex(),
		"assets_hash":      leaf.AssetsHash.Hex(),
		"leaf":             leaf.Leaf.Hex(),
		"leaf_index":       leaf.Index,
		"proof":            hexProof,
		"assets":           addrs,
		"verify_with":      "HintRegistry.verifyInclusion(epochId, account, assetsHash, proof)",
		"onchain_epoch_id": e.OnchainID,
	})
}

// --------------------------------------------------------------------------
// Candidates: contracts discovered at the head
// --------------------------------------------------------------------------

type candidateJSON struct {
	Address         string  `json:"address"`
	Standard        string  `json:"standard"`
	FirstSeenBlock  uint64  `json:"first_seen_block"`
	LastSeenBlock   uint64  `json:"last_seen_block"`
	EventCount      uint64  `json:"event_count"`
	BlocksSeen      uint64  `json:"blocks_seen"`
	Promotable      bool    `json:"promotable"`
	Promoted        bool    `json:"promoted"`
	PromotedAt      *string `json:"promoted_at,omitempty"`
	PromotionReason string  `json:"promotion_reason,omitempty"`
	Spam            bool    `json:"spam"`
	SpamAt          *string `json:"spam_at,omitempty"`
	SpamReason      string  `json:"spam_reason,omitempty"`
	// Verdict collapses the two marks into the one string a UI renders as a badge:
	// "approved", "spam", or absent for a contract nobody has judged.
	Verdict string `json:"verdict,omitempty"`
	// Symbol and Name are read at head, not stored: discovery counts events and
	// never probes metadata, so a candidate row has none. They are best-effort —
	// a token that does not answer, or a node that will not run the batch, leaves
	// them empty rather than failing the listing.
	Symbol   string `json:"symbol,omitempty"`
	Name     string `json:"name,omitempty"`
	Decimals *int16 `json:"decimals,omitempty"`
}

// listCandidates returns contracts seen at the head that are not indexed yet.
//
// These are counters only — no per-account data exists for a candidate. That is the
// trade this design makes: watching every contract is cheap, indexing one is not.
func (s *Server) listCandidates(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}

	ctx := r.Context()
	rows, err := s.d.Store.ListCandidates(ctx, chainID, store.CandidateFilter{
		IncludePromoted: r.URL.Query().Get("include_promoted") == "true",
		IncludeSpam:     r.URL.Query().Get("include_spam") == "true",
		Limit:           intParam(r, "limit", 50, 1, 500),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	var minEvents, minBlocks uint64
	if wk, ok := s.d.Chains.Worker(chainID); ok {
		minEvents, minBlocks = wk.DiscoveryThresholds()
	}

	out := make([]candidateJSON, len(rows))
	for i, c := range rows {
		out[i] = candidateView(c, minEvents, minBlocks)
	}
	s.decorateCandidates(ctx, chainID, out)

	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":   chainID,
		"candidates": out,
		"thresholds": map[string]uint64{"min_events": minEvents, "min_blocks": minBlocks},
	})
}

type promoteRequest struct {
	Reason string `json:"reason"`
}

// promoteCandidate commits a discovered contract to being indexed, which starts its
// backfill down to the node's history horizon.
func (s *Server) promoteCandidate(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	addr, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	worker, ok := s.d.Chains.Worker(chainID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown chain", nil)
		return
	}

	var req promoteRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad request body", err)
			return
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = "manual: promoted via API"
	}

	ctx := r.Context()
	if err := worker.Promote(ctx, addr, reason); err != nil {
		writeErr(w, http.StatusBadRequest, "promotion failed", err)
		return
	}

	a, err := s.d.Store.GetAsset(ctx, chainID, addr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"promoted": true,
		"reason":   reason,
		"asset":    s.assetView(ctx, a),
	})
}

// candidateView renders one observed contract. Promotable is a live question, not a
// stored flag: it is the thresholds the worker is running with right now, and a
// contract someone has already judged is never offered again.
func candidateView(c store.Candidate, minEvents, minBlocks uint64) candidateJSON {
	out := candidateJSON{
		Address:         c.Address.Hex(),
		Standard:        standardName(c.Standard),
		FirstSeenBlock:  c.FirstSeenBlock,
		LastSeenBlock:   c.LastSeenBlock,
		EventCount:      c.EventCount,
		BlocksSeen:      c.BlocksSeen,
		Promotable:      c.SpamAt == nil && c.PromotedAt == nil && c.EventCount >= minEvents && c.BlocksSeen >= minBlocks,
		Promoted:        c.PromotedAt != nil,
		PromotionReason: c.PromotionReason,
		Spam:            c.SpamAt != nil,
		SpamReason:      c.SpamReason,
	}
	if c.PromotedAt != nil {
		ts := c.PromotedAt.UTC().Format(time.RFC3339)
		out.PromotedAt, out.Verdict = &ts, "approved"
	}
	if c.SpamAt != nil {
		ts := c.SpamAt.UTC().Format(time.RFC3339)
		out.SpamAt, out.Verdict = &ts, "spam"
	}
	return out
}

// decorateCandidates fills in symbol/name/decimals from the head, in one batched call
// for the whole page. Best-effort by design: a token that will not answer leaves the
// fields empty rather than failing the listing.
func (s *Server) decorateCandidates(ctx context.Context, chainID uint64, out []candidateJSON) {
	src, ok := s.d.Chains.Source(chainID)
	if !ok || len(out) == 0 {
		return
	}
	addrs := make([]common.Address, len(out))
	for i := range out {
		addrs[i] = common.HexToAddress(out[i].Address)
	}
	meta := s.meta.lookup(ctx, src, chainID, addrs)
	for i := range out {
		if m, ok := meta[common.HexToAddress(out[i].Address)]; ok {
			out[i].Symbol, out[i].Name, out[i].Decimals = m.Symbol, m.Name, m.Decimals
		}
	}
}

// markCandidateSpam records that a contract is not worth indexing. Unlike a promotion
// this spends nothing — it only stops discovery from offering the contract again — but
// it is still an operator's judgement written into the index, so it is guarded.
func (s *Server) markCandidateSpam(w http.ResponseWriter, r *http.Request) {
	s.candidateVerdict(w, r, true)
}

// clearCandidateSpam is the undo. A POST rather than a DELETE on the same path: the
// CORS preflight this server answers allows GET, POST and OPTIONS only, and the auth
// guard covers POSTs, so a DELETE would be both unreachable cross-origin and
// unauthenticated where it did arrive.
func (s *Server) clearCandidateSpam(w http.ResponseWriter, r *http.Request) {
	s.candidateVerdict(w, r, false)
}

func (s *Server) candidateVerdict(w http.ResponseWriter, r *http.Request, spam bool) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	addr, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}

	var req promoteRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad request body", err)
			return
		}
	}
	reason := req.Reason
	if reason == "" {
		reason = "manual: marked spam via API"
	}

	ctx := r.Context()
	if spam {
		err = s.d.Store.MarkCandidateSpam(ctx, chainID, addr, reason)
	} else {
		err = s.d.Store.ClearCandidateSpam(ctx, chainID, addr)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such candidate", nil)
		return
	case errors.Is(err, store.ErrAlreadyPromoted):
		writeErr(w, http.StatusConflict, "already promoted",
			errors.New("the contract is an indexed asset; revoke the asset instead"))
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "verdict failed", err)
		return
	}

	// Answer with the row as it now stands, so a caller can patch it in place instead
	// of refetching the whole listing.
	c, err := s.d.Store.GetCandidate(ctx, chainID, addr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	var minEvents, minBlocks uint64
	if wk, ok := s.d.Chains.Worker(chainID); ok {
		minEvents, minBlocks = wk.DiscoveryThresholds()
	}
	out := []candidateJSON{candidateView(c, minEvents, minBlocks)}
	s.decorateCandidates(ctx, chainID, out)
	writeJSON(w, http.StatusOK, out[0])
}

// --------------------------------------------------------------------------
// Decisions: the ledger of verdicts passed on discovered contracts
// --------------------------------------------------------------------------

type decisionJSON struct {
	Address  string `json:"address"`
	Standard string `json:"standard"`
	Symbol   string `json:"symbol,omitempty"`
	Name     string `json:"name,omitempty"`
	Verdict  string `json:"verdict"`
	At       string `json:"at"`
	Reason   string `json:"reason,omitempty"`
}

// listDecisions returns recent verdicts, newest first.
//
// Scoped to contracts *discovery* found: a hint someone paid to register on-chain
// enters the index through the registry mirror without ever becoming a candidate, so it
// has no verdict to report here. The ledger answers "what has the curator ruled", not
// "what is in the index".
func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	ctx := r.Context()
	rows, err := s.d.Store.RecentDecisions(ctx, chainID, intParam(r, "limit", 50, 1, 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	out := make([]decisionJSON, len(rows))
	for i, d := range rows {
		out[i] = decisionJSON{
			Address:  d.Address.Hex(),
			Standard: standardName(d.Standard),
			Symbol:   d.Symbol,
			Name:     d.Name,
			Verdict:  d.Verdict,
			At:       d.At.UTC().Format(time.RFC3339),
			Reason:   d.Reason,
		}
	}

	// Only the rows the join left blank — spam contracts, which were never probed —
	// need the head. Promotions already carry their metadata.
	if src, ok := s.d.Chains.Source(chainID); ok {
		var missing []common.Address
		for i := range out {
			if out[i].Symbol == "" && out[i].Name == "" {
				missing = append(missing, common.HexToAddress(out[i].Address))
			}
		}
		if len(missing) > 0 {
			meta := s.meta.lookup(ctx, src, chainID, missing)
			for i := range out {
				if m, ok := meta[common.HexToAddress(out[i].Address)]; ok {
					out[i].Symbol, out[i].Name = m.Symbol, m.Name
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"chain_id": chainID, "decisions": out})
}
