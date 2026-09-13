package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Skanislav/evm-scan/internal/store"
)

// A verdict: a reader's signed split of their own holdings.
//
// The page reads a wallet, sorts it into recognized and not, lets the reader flip
// what it got wrong, and asks the wallet for one EIP-712 signature over the whole
// split (docs/SHIP.md §2). This route takes the pairs and the signature, recovers
// the signer, checks it is the account, checks the deadline is later than the last
// one stored for this (chain, voter), and replaces the voter's rows with the split.
// Everyone's next lookup is ordered by what was signed; a recognized-but-unindexed
// contract with enough net signers is promoted by the next discovery tick.
//
// Like a vote, a verdict is a priority signal and nothing else: it orders a list,
// it never changes what a balance read says, and a spam verdict from the operator
// still outranks it. The voter is stored blinded, the same as /v1/demand, so the
// table counts an account once and cannot be walked back to who holds what.
//
// Open on purpose (guarded() allowlists it): the signature is the credential,
// checked here, and the operator's token would only stop readers.

type verdictPairJSON struct {
	Address string `json:"address"`
	Weight  int8   `json:"weight"`
}

type verdictRequest struct {
	ChainID   uint64            `json:"chain_id"`
	Account   string            `json:"account"`
	Deadline  string            `json:"deadline"`
	Verdicts  []verdictPairJSON `json:"verdicts"`
	Signature string            `json:"signature"`
}

func (s *Server) postVerdict(w http.ResponseWriter, r *http.Request) {
	var req verdictRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	// The chain is inside the signed message, so it is never defaulted here: a
	// verdict signed for chain 1 must not land on whatever chain is configured first.
	if req.ChainID == 0 {
		writeErr(w, http.StatusBadRequest, "chain_id is required", nil)
		return
	}
	if !common.IsHexAddress(req.Account) {
		writeErr(w, http.StatusBadRequest, "account must be an address", nil)
		return
	}
	account := common.HexToAddress(req.Account)
	if len(req.Verdicts) > store.MaxDemandPerVote {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a verdict names at most %d contracts", store.MaxDemandPerVote), nil)
		return
	}
	pairs := make([]store.Verdict, 0, len(req.Verdicts))
	seen := make(map[common.Address]bool, len(req.Verdicts))
	for _, v := range req.Verdicts {
		if !common.IsHexAddress(v.Address) {
			writeErr(w, http.StatusBadRequest, "verdicts[].address must be an address", nil)
			return
		}
		if v.Weight != 1 && v.Weight != -1 {
			// 0 is "not in the list"; sending it would make absence and presence
			// two spellings of the same thing, and the digest would then depend on
			// which one the page chose.
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("weight must be 1 or -1, got %d", v.Weight), nil)
			return
		}
		a := common.HexToAddress(v.Address)
		if seen[a] {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s appears twice", a.Hex()), nil)
			return
		}
		seen[a] = true
		pairs = append(pairs, store.Verdict{Address: a, Weight: v.Weight})
	}
	deadline, ok := new(big.Int).SetString(req.Deadline, 10)
	if !ok || deadline.Sign() < 0 || deadline.BitLen() > 256 {
		writeErr(w, http.StatusBadRequest, "deadline must be a decimal unix time", nil)
		return
	}
	if deadline.Cmp(big.NewInt(time.Now().Unix())) <= 0 {
		writeErr(w, http.StatusBadRequest, "the signature has expired", nil)
		return
	}
	sig, err := hexutil.Decode(req.Signature)
	if err != nil || len(sig) != 65 {
		writeErr(w, http.StatusBadRequest, "signature must be 65 bytes of hex", err)
		return
	}
	digest := verdictDigest(pairs)
	who, err := recoverVerdictSigner(account, req.ChainID, digest, deadline, sig)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad signature", err)
		return
	}
	if who != account {
		writeErr(w, http.StatusUnauthorized, fmt.Sprintf("signed by %s, not by the account", who.Hex()), nil)
		return
	}

	recorded, cleared, err := s.d.Store.ReplaceVerdicts(r.Context(), req.ChainID, account, deadline, pairs)
	if errors.Is(err, store.ErrStaleVerdict) {
		writeErr(w, http.StatusConflict, "a verdict with a later deadline is already stored; sign a new one", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record", err)
		return
	}
	_, runs := s.d.Chains.Worker(req.ChainID)
	s.d.Log.Info("recorded a signed verdict", "chain_id", req.ChainID, "recorded", recorded, "cleared", cleared, "indexed_here", runs)
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id": req.ChainID,
		"account":  account.Hex(),
		"digest":   digest.Hex(),
		"deadline": deadline.String(),
		"recorded": recorded,
		"cleared":  cleared,
		// indexed_here says whether this deployment can act on the verdict itself.
		// A false is not a refusal: the split is kept, orders what this deployment
		// serves, and tells an operator which chain readers want next.
		"indexed_here": runs,
	})
}
