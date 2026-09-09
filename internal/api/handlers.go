package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/indexer"
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
	writeJSON(w, http.StatusOK, map[string]any{"chain_id": chainID, "assets": out})
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
	if chainID == 0 && len(s.chains) == 1 {
		chainID = s.chains[0]
	}
	src, ok := s.d.Sources[chainID]
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
	if wk, ok := s.d.Workers[chainID]; ok {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"account":     account.Hex(),
		"chain_id":    chainID,
		"as_of_block": head,
		"assets":      out,
	})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"account":     account.Hex(),
		"chain_id":    chainID,
		"as_of_block": head,
		"contracts":   hints,
		"disclaimer":  "discovery hint over registered assets only; verify against the chain",
	})
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
	out := make([]epochJSON, 0, len(rows))
	for _, e := range rows {
		out = append(out, epochView(e))
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
	if chainID == 0 && len(s.chains) == 1 {
		chainID = s.chains[0]
	}
	if _, ok := s.d.Sources[chainID]; !ok {
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
	rows, err := s.d.Store.ListCandidates(ctx, chainID,
		r.URL.Query().Get("include_promoted") == "true",
		intParam(r, "limit", 50, 1, 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	var minEvents, minBlocks uint64
	if wk, ok := s.d.Workers[chainID]; ok {
		minEvents, minBlocks = wk.DiscoveryThresholds()
	}

	out := make([]candidateJSON, len(rows))
	for i, c := range rows {
		out[i] = candidateJSON{
			Address:         c.Address.Hex(),
			Standard:        standardName(c.Standard),
			FirstSeenBlock:  c.FirstSeenBlock,
			LastSeenBlock:   c.LastSeenBlock,
			EventCount:      c.EventCount,
			BlocksSeen:      c.BlocksSeen,
			Promotable:      c.EventCount >= minEvents && c.BlocksSeen >= minBlocks,
			Promoted:        c.PromotedAt != nil,
			PromotionReason: c.PromotionReason,
		}
		if c.PromotedAt != nil {
			ts := c.PromotedAt.UTC().Format(time.RFC3339)
			out[i].PromotedAt = &ts
		}
	}

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
	worker, ok := s.d.Workers[chainID]
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
