package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/store"
)

// Demand: a reader's vote for what to index next.
//
// The lookup page shows an account's holdings, and the ones the index does not keep
// can be voted for from there. A vote is a priority signal and nothing else: it
// orders the promotion queue and, with min_voters set, lets a contract enough
// accounts asked for be promoted without waiting on activity. It never changes what
// a balance read says and a spam verdict still outranks it.
//
// What the daemon learns is the account and the contracts it voted for, which is
// what a hosted lookup already tells it. The voter is stored blinded under a
// per-deployment salt (store.RecordDemand), so the table counts an account once
// and cannot be walked back to who holds what.

type demandRequest struct {
	ChainID uint64   `json:"chain_id"`
	Account string   `json:"account"`
	Assets  []string `json:"assets"`
}

type demandJSON struct {
	Address       string `json:"address"`
	Voters        uint64 `json:"voters"`
	OnchainVoters uint64 `json:"onchain_voters"`
	LastAt        string `json:"last_at"`
	Indexed       bool   `json:"indexed"`
	Candidate     bool   `json:"candidate"`
	Spam          bool   `json:"spam"`
	EventCount    uint64 `json:"event_count"`
	// Promotable says whether the vote count alone clears min_voters; false when
	// demand promotion is off for the chain.
	Promotable bool `json:"promotable"`
}

func (s *Server) recordDemand(w http.ResponseWriter, r *http.Request) {
	var req demandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	// A vote may name a chain this deployment does not run. The cross-chain sweep
	// finds holdings on a dozen chains and the index runs on one; demand for the
	// others is still demand — it is what tells an operator which chain to add next
	// (POST /v1/chains), and DemandedUnseen promotes it the moment that chain runs.
	// Without a chain_id the vote lands on the first configured chain, as every
	// other route defaults.
	chainID := req.ChainID
	if chainID == 0 {
		e, ok := s.d.Chains.First()
		if !ok {
			writeErr(w, http.StatusBadRequest, "no chains are configured", nil)
			return
		}
		chainID = e.ID
	}
	if !common.IsHexAddress(req.Account) {
		writeErr(w, http.StatusBadRequest, "account must be an address", nil)
		return
	}
	if len(req.Assets) == 0 {
		writeErr(w, http.StatusBadRequest, "assets is empty", nil)
		return
	}
	if len(req.Assets) > store.MaxDemandPerVote {
		writeErr(w, http.StatusBadRequest, "too many assets in one vote", nil)
		return
	}
	addrs := make([]common.Address, 0, len(req.Assets))
	for _, a := range req.Assets {
		if !common.IsHexAddress(a) {
			writeErr(w, http.StatusBadRequest, "assets must be addresses", nil)
			return
		}
		addrs = append(addrs, common.HexToAddress(a))
	}

	ctx := r.Context()
	added, err := s.d.Store.RecordDemand(ctx, chainID, common.HexToAddress(req.Account), addrs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record", err)
		return
	}
	totals, err := s.d.Store.DemandFor(ctx, chainID, addrs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	voters := make(map[string]uint64, len(totals))
	for a, n := range totals {
		voters[a.Hex()] = n
	}
	var minVoters uint64
	wk, runs := s.d.Chains.Worker(chainID)
	if runs {
		_, _, minVoters = wk.DiscoveryThresholds()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":   chainID,
		"recorded":   added,
		"voters":     voters,
		"min_voters": minVoters,
		// indexed_here says whether this deployment can act on the vote itself. A
		// false is not a refusal: the count is kept, and the on-chain vote — the one
		// every indexer mirrors — is the same either way.
		"indexed_here": runs,
	})
}

// demandChain reads chain_id for the demand routes. Unlike chainOf it accepts a
// chain this deployment does not run, since demand is recorded for those too.
func (s *Server) demandChain(r *http.Request) (uint64, error) {
	raw := r.URL.Query().Get("chain_id")
	if raw == "" {
		e, ok := s.d.Chains.First()
		if !ok {
			return 0, fmt.Errorf("no chains are configured")
		}
		return e.ID, nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("chain_id %q is not a chain id", raw)
	}
	return id, nil
}

func (s *Server) listDemand(w http.ResponseWriter, r *http.Request) {
	chainID, err := s.demandChain(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	rows, err := s.d.Store.ListDemand(r.Context(), chainID, intParam(r, "limit", 50, 1, 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	var minVoters uint64
	if wk, ok := s.d.Chains.Worker(chainID); ok {
		_, _, minVoters = wk.DiscoveryThresholds()
	}
	out := make([]demandJSON, len(rows))
	for i, d := range rows {
		out[i] = demandJSON{
			Address: d.Address.Hex(), Voters: d.Voters, OnchainVoters: d.OnchainVoters,
			LastAt:  d.LastAt.UTC().Format(time.RFC3339),
			Indexed: d.Indexed, Candidate: d.Candidate, Spam: d.Spam, EventCount: d.EventCount,
			Promotable: minVoters > 0 && d.Voters >= minVoters && !d.Indexed && !d.Spam,
		}
	}
	_, runs := s.d.Chains.Worker(chainID)
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":     chainID,
		"min_voters":   minVoters,
		"indexed_here": runs,
		"demand":       out,
	})
}

// --------------------------------------------------------------------------
// The relay: a vote the voter signed and somebody else pays to land.
//
// A reader who wants their vote on the registry rather than only in this
// deployment's table needs gas on the registry's chain, which most wallets that
// hold mainnet tokens do not have. So the page asks the wallet for an EIP-712
// signature over Vote(voter, chainId, tokens, nonce, deadline) instead, and this
// route carries it to HintRegistry.voteFor with the publisher's key. The contract
// recovers the signer and counts the vote as theirs; the carrier is nobody.
//
// What the relay spends is gas, so it is stingy in three ways: at most
// relayMaxTokens per vote, one relayed transaction per voter per chain voted for
// per relayWindow, and a simulation before every send so a bad signature or an
// expired deadline costs a call and not a transaction. It refuses a vote that
// would count nothing new, because the contract still spends the nonce and the
// gas on those.
//
// The window is per chain because Vote is: the struct carries one chainId, so a
// wallet the sweep found on four chains signs four votes, and they have to land
// one after another — each spends nonces(voter), so the next cannot be simulated,
// let alone sent, until the last has mined. That is why the route waits for the
// receipt before answering: the page asks for the nonce again and signs the next
// vote against a chain state that already has the previous one in it.
// --------------------------------------------------------------------------

const (
	relayMaxTokens = 20
	relayWindow    = 10 * time.Minute
	relayMinedWait = 90 * time.Second
)

type relayKey struct {
	voter   common.Address
	chainID uint64
}

var relayLast = struct {
	sync.Mutex
	at map[relayKey]time.Time
}{at: map[relayKey]time.Time{}}

type relayRequest struct {
	ChainID   uint64   `json:"chain_id"`
	Voter     string   `json:"voter"`
	Tokens    []string `json:"tokens"`
	Nonce     string   `json:"nonce"`
	Deadline  string   `json:"deadline"`
	Signature string   `json:"signature"`
}

// relayInfo tells the page what to sign: the EIP-712 domain, the voter's next
// nonce, and whether this deployment carries votes at all.
func (s *Server) relayInfo(w http.ResponseWriter, r *http.Request) {
	if s.d.Registry == nil {
		writeErr(w, http.StatusNotFound, "no registry", nil)
		return
	}
	out := map[string]any{
		"available":      s.d.Relay != nil,
		"max_tokens":     relayMaxTokens,
		"window_seconds": int(relayWindow.Seconds()),
		"domain": map[string]any{
			"name": "HintRegistry", "version": "1",
			"chainId": s.d.RegistryChainID, "verifyingContract": s.d.Registry.Address().Hex(),
		},
		"types": map[string]any{
			"Vote": []map[string]string{
				{"name": "voter", "type": "address"}, {"name": "chainId", "type": "uint64"},
				{"name": "tokens", "type": "address[]"}, {"name": "nonce", "type": "uint256"},
				{"name": "deadline", "type": "uint256"},
			},
		},
	}
	if v := r.URL.Query().Get("voter"); v != "" {
		if !common.IsHexAddress(v) {
			writeErr(w, http.StatusBadRequest, "voter must be an address", nil)
			return
		}
		n, err := s.d.Registry.VoteNonce(r.Context(), common.HexToAddress(v))
		if err != nil {
			// A registry deployed before voteFor has no nonces(); that is "no relay",
			// and the page sends the vote from the wallet instead.
			out["available"] = false
			out["reason"] = "this registry predates signed votes"
		} else {
			out["nonce"] = n.String()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) relayVote(w http.ResponseWriter, r *http.Request) {
	if s.d.Registry == nil || s.d.Relay == nil {
		writeErr(w, http.StatusServiceUnavailable, "this deployment carries no votes; send the transaction from your own wallet", nil)
		return
	}
	var req relayRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	if !common.IsHexAddress(req.Voter) {
		writeErr(w, http.StatusBadRequest, "voter must be an address", nil)
		return
	}
	if len(req.Tokens) == 0 || len(req.Tokens) > relayMaxTokens {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a relayed vote names 1 to %d tokens", relayMaxTokens), nil)
		return
	}
	if req.ChainID == 0 {
		writeErr(w, http.StatusBadRequest, "chain_id is required", nil)
		return
	}
	tokens := make([]common.Address, 0, len(req.Tokens))
	for _, t := range req.Tokens {
		if !common.IsHexAddress(t) {
			writeErr(w, http.StatusBadRequest, "tokens must be addresses", nil)
			return
		}
		tokens = append(tokens, common.HexToAddress(t))
	}
	nonce, ok := new(big.Int).SetString(req.Nonce, 10)
	if !ok {
		writeErr(w, http.StatusBadRequest, "nonce must be a decimal integer", nil)
		return
	}
	deadline, ok := new(big.Int).SetString(req.Deadline, 10)
	if !ok {
		writeErr(w, http.StatusBadRequest, "deadline must be a decimal unix time", nil)
		return
	}
	if deadline.Cmp(big.NewInt(time.Now().Unix())) < 0 {
		writeErr(w, http.StatusBadRequest, "the signature has expired", nil)
		return
	}
	sig := common.FromHex(req.Signature)
	if len(sig) != 65 {
		writeErr(w, http.StatusBadRequest, "signature must be 65 bytes", nil)
		return
	}
	voter := common.HexToAddress(req.Voter)
	ctx := r.Context()

	// One transaction per voter per chain per window, before anything is read or sent.
	rk := relayKey{voter, req.ChainID}
	relayLast.Lock()
	if last, seen := relayLast.at[rk]; seen && time.Since(last) < relayWindow {
		relayLast.Unlock()
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("one relayed vote per %s per voter and chain", relayWindow), nil)
		return
	}
	relayLast.Unlock()

	// The nonce the contract expects, and whether any token is new. The signature
	// covers the whole list, so nothing can be dropped from it; a vote that would
	// count nothing is refused instead of paid for.
	want, err := s.d.Registry.VoteNonce(ctx, voter)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not read the nonce", err)
		return
	}
	if want.Cmp(nonce) != 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("nonce %s is not the voter's next (%s); sign again", nonce, want), nil)
		return
	}
	fresh := 0
	for _, t := range tokens {
		voted, err := s.d.Registry.HasVoted(ctx, hintreg.AssetKey(req.ChainID, t), voter)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "could not read the registry", err)
			return
		}
		if !voted {
			fresh++
		}
	}
	if fresh == 0 {
		writeErr(w, http.StatusConflict, "this voter already counts for every token named", nil)
		return
	}

	// Simulate as the relay would send it: the contract recovers the signer, so a
	// signature from anyone but the voter reverts here, for the price of a call.
	if err := s.d.Registry.Simulate(ctx, s.d.Relay.Sender(), "voteFor", voter, req.ChainID, tokens, deadline, sig); err != nil {
		writeErr(w, http.StatusBadRequest, "the registry rejected the signed vote", err)
		return
	}
	data, err := s.d.Registry.ABI().Pack("voteFor", voter, req.ChainID, tokens, deadline, sig)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pack failed", err)
		return
	}

	relayLast.Lock()
	relayLast.at[rk] = time.Now()
	relayLast.Unlock()
	tx, err := s.d.Relay.Submit(ctx, s.d.Registry.Address(), nil, data)
	if err != nil {
		relayLast.Lock()
		delete(relayLast.at, rk)
		relayLast.Unlock()
		writeErr(w, http.StatusBadGateway, "could not send", err)
		return
	}
	s.d.Log.Info("relayed a signed vote", "voter", voter.Hex(), "chain_id", req.ChainID, "tokens", len(tokens), "new", fresh, "tx", tx.Hex())

	// Sent is not counted: the nonce moves when the transaction mines, and the
	// voter's next vote (for their next chain) cannot be signed against anything
	// else. Wait for the receipt, bounded, and say which it was.
	wctx, cancel := context.WithTimeout(ctx, relayMinedWait)
	defer cancel()
	out := map[string]any{
		"tx":             tx.Hex(),
		"voter":          voter.Hex(),
		"tokens":         len(tokens),
		"new":            fresh,
		"registry":       s.d.Registry.Address().Hex(),
		"chain_id":       s.d.RegistryChainID,
		"voted_chain_id": req.ChainID,
		"mined":          false,
	}
	if rcpt, err := s.d.Relay.Wait(wctx, tx); err == nil && rcpt != nil {
		out["mined"] = rcpt.Status == 1
		out["block"] = rcpt.BlockNumber.Uint64()
		if rcpt.Status != 1 {
			out["error"] = "the transaction reverted"
		}
	}
	writeJSON(w, http.StatusOK, out)
}
