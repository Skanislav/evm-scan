package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/contracts"
)

// A readable name, claimed by the reader and paid for by this deployment.
//
// Every account already has a name under the hints label and it is forty hex
// characters. HintAliasResolver lets an account point one readable label at
// itself, and the claim is an EIP-712 signature rather than a transaction for the
// same reason the vote relay exists: a wallet holding mainnet tokens generally
// holds no gas on the registry's chain, and asking it to acquire some to publish
// its own name is where the flow would end.
//
// So the reader signs Claim(label, account, nonce, deadline) and this route sends
// it with the publisher's key. The contract recovers the signer; the carrier is
// nobody and is not recorded. What this deployment spends is gas, so the route is
// stingy the same three ways the vote relay is: one transaction per account per
// window, a simulation before every send so a bad signature costs a call rather
// than a transaction, and a wait for the receipt, because the nonce only moves
// when the claim mines and the account's next signature has to be made against a
// state that already holds this one.
//
// The name is the signer's and nothing else follows from it. A signature proves
// the claimant controls the address; it says nothing about whether the label
// describes them. The contract serves names first come, first served, and so does
// this.

const (
	nameRelayWindow    = 10 * time.Minute
	nameRelayMinedWait = 90 * time.Second
)

var nameRelayLast = struct {
	sync.Mutex
	at map[common.Address]time.Time
}{at: map[common.Address]time.Time{}}

// aliasABI is the HintAliasResolver ABI, loaded once. The daemon never deploys
// this contract; it only carries claims to one an operator has already deployed.
var aliasABI = func() abi.ABI {
	art, err := contracts.Load("HintAliasResolver")
	if err != nil {
		panic(err)
	}
	a, err := art.Parsed()
	if err != nil {
		panic(err)
	}
	return a
}()

type nameRequest struct {
	Label     string `json:"label"`
	Account   string `json:"account"`
	Deadline  string `json:"deadline"`
	Signature string `json:"signature"`
	// Release asks for releaseFor instead of claimFor. A reader who published a
	// name must be able to withdraw it without holding gas either.
	Release bool `json:"release"`
}

// nameNonce reads the account's next nonce from the resolver.
func (s *Server) nameNonce(ctx context.Context, account common.Address) (*big.Int, error) {
	data, err := aliasABI.Pack("nonces", account)
	if err != nil {
		return nil, err
	}
	out, err := s.callNameResolver(ctx, data)
	if err != nil {
		return nil, err
	}
	vals, err := aliasABI.Unpack("nonces", out)
	if err != nil {
		return nil, err
	}
	return vals[0].(*big.Int), nil
}

func (s *Server) callNameResolver(ctx context.Context, data []byte) ([]byte, error) {
	src, ok := s.d.Chains.Source(s.d.RegistryChainID)
	if !ok {
		return nil, fmt.Errorf("registry chain %d is not running", s.d.RegistryChainID)
	}
	to := s.d.NameResolver
	return src.CallAtHead(ctx, ethereum.CallMsg{To: &to, Data: data})
}

// nameInfo tells the page what to sign: the EIP-712 domain, the account's next
// nonce, and whether this deployment carries claims at all.
func (s *Server) nameInfo(w http.ResponseWriter, r *http.Request) {
	if s.d.NameResolver == (common.Address{}) {
		writeErr(w, http.StatusNotFound, "this deployment serves no readable names", nil)
		return
	}
	out := map[string]any{
		"available":      s.d.Relay != nil,
		"resolver":       s.d.NameResolver.Hex(),
		"parent":         s.d.ENSParent,
		"window_seconds": int(nameRelayWindow.Seconds()),
		"domain": map[string]any{
			"name": "evm-scan hint name", "version": "1",
			"chainId": s.d.RegistryChainID, "verifyingContract": s.d.NameResolver.Hex(),
		},
		"types": map[string]any{
			"Claim": []map[string]string{
				{"name": "label", "type": "string"}, {"name": "account", "type": "address"},
				{"name": "nonce", "type": "uint256"}, {"name": "deadline", "type": "uint256"},
			},
			"Release": []map[string]string{
				{"name": "label", "type": "string"}, {"name": "account", "type": "address"},
				{"name": "nonce", "type": "uint256"}, {"name": "deadline", "type": "uint256"},
			},
		},
	}
	if v := r.URL.Query().Get("account"); v != "" {
		if !common.IsHexAddress(v) {
			writeErr(w, http.StatusBadRequest, "account must be an address", nil)
			return
		}
		n, err := s.nameNonce(r.Context(), common.HexToAddress(v))
		if err != nil {
			out["available"] = false
			out["reason"] = "the configured resolver does not answer nonces()"
		} else {
			out["nonce"] = n.String()
		}
	}
	if label := r.URL.Query().Get("label"); label != "" {
		who, err := s.nameHolder(r.Context(), label)
		if err == nil {
			out["label"] = label
			out["held_by"] = who.Hex()
			out["free"] = who == common.Address{}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) nameHolder(ctx context.Context, label string) (common.Address, error) {
	data, err := aliasABI.Pack("accountOfLabel", labelHash(label))
	if err != nil {
		return common.Address{}, err
	}
	out, err := s.callNameResolver(ctx, data)
	if err != nil {
		return common.Address{}, err
	}
	vals, err := aliasABI.Unpack("accountOfLabel", out)
	if err != nil {
		return common.Address{}, err
	}
	return vals[0].(common.Address), nil
}

// relayName carries one signed claim or release.
func (s *Server) relayName(w http.ResponseWriter, r *http.Request) {
	if s.d.NameResolver == (common.Address{}) || s.d.Relay == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this deployment carries no name claims; send the transaction from your own wallet", nil)
		return
	}
	var req nameRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	if !common.IsHexAddress(req.Account) {
		writeErr(w, http.StatusBadRequest, "account must be an address", nil)
		return
	}
	account := common.HexToAddress(req.Account)
	// The contract is the authority on what a label may be; this is only here so
	// an obviously bad one costs nothing to refuse.
	if req.Label == "" || len(req.Label) > 63 || req.Label != strings.ToLower(req.Label) {
		writeErr(w, http.StatusBadRequest, "a label is 1 to 63 lowercase characters", nil)
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
	ctx := r.Context()

	// One transaction per account per window, before anything is read or sent.
	nameRelayLast.Lock()
	if last, seen := nameRelayLast.at[account]; seen && time.Since(last) < nameRelayWindow {
		nameRelayLast.Unlock()
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("one relayed claim per %s per account", nameRelayWindow), nil)
		return
	}
	nameRelayLast.Unlock()

	method := "claimFor"
	if req.Release {
		method = "releaseFor"
	}

	// Simulate as the relay would send it. The contract recovers the signer, so a
	// signature from anyone but the account reverts here for the price of a call —
	// and so does a taken label, a spent nonce and an expired deadline.
	data, err := aliasABI.Pack(method, req.Label, account, deadline, sig)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pack failed", err)
		return
	}
	src, okSrc := s.d.Chains.Source(s.d.RegistryChainID)
	if !okSrc {
		writeErr(w, http.StatusServiceUnavailable, "the registry chain is not running", nil)
		return
	}
	to := s.d.NameResolver
	from := s.d.Relay.Sender()
	if _, err := src.CallAtHead(ctx, ethereum.CallMsg{From: from, To: &to, Data: data}); err != nil {
		writeErr(w, http.StatusBadRequest, "the resolver rejected the signed claim", err)
		return
	}

	nameRelayLast.Lock()
	nameRelayLast.at[account] = time.Now()
	nameRelayLast.Unlock()
	tx, err := s.d.Relay.Submit(ctx, s.d.NameResolver, nil, data)
	if err != nil {
		nameRelayLast.Lock()
		delete(nameRelayLast.at, account)
		nameRelayLast.Unlock()
		writeErr(w, http.StatusBadGateway, "could not send", err)
		return
	}
	s.d.Log.Info("relayed a signed name claim",
		"account", account.Hex(), "label", req.Label, "release", req.Release, "tx", tx.Hex())

	out := map[string]any{"tx": tx.Hex(), "label": req.Label, "account": account.Hex()}
	if s.d.ENSParent != "" && !req.Release {
		out["name"] = req.Label + ".hints." + s.d.ENSParent
	}
	wctx, cancel := context.WithTimeout(ctx, nameRelayMinedWait)
	defer cancel()
	if _, err := s.d.Relay.Wait(wctx, tx); err != nil {
		out["mined"] = false
		out["note"] = "sent, but the receipt did not arrive in time; read the name before signing another"
		writeJSON(w, http.StatusAccepted, out)
		return
	}
	out["mined"] = true
	writeJSON(w, http.StatusOK, out)
}

func labelHash(label string) [32]byte {
	return [32]byte(crypto.Keccak256([]byte(label)))
}
