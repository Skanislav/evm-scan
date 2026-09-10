package api

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/Skanislav/evm-scan/contracts"
)

// lensArtifact is everything a client needs to run a lens itself.
//
// The lens is deployless: eth_call with no `to` executes creation code and returns
// whatever the constructor returns, so the whole query is `creation || abi.encode(
// request)`. Nothing about that is privileged, which means a browser holding these
// three fields can make the identical call against whatever RPC its user trusts and
// get the identical answer — the computation happens inside the EVM, not in this
// daemon.
//
// Serving it matters for cost as much as for principle. A lens call through a light
// client is the most expensive thing this deployment does: Helios re-verifies every
// storage slot the lens touches with eth_getProof, so one portfolio read fans out
// into many billed upstream requests. A user reading their own balances through
// their own endpoint moves that spend off the deployment entirely, and the answer is
// no less trustworthy for it — it is their node, and the bytecode is right here to
// check.
//
// The shapes are taken from the compiled artifacts rather than transcribed, because
// a hand-copied ABI is the classic way for a client and a server to drift apart
// silently.
type lensArtifact struct {
	Name string `json:"name"`
	// Creation is the deployless payload's prefix, hex with 0x.
	Creation string `json:"creation"`
	// Request is the constructor's inputs: the ABI parameters to encode and append.
	Request json.RawMessage `json:"request"`
	// Reply is what the call returns, taken from the interface's query method so it
	// cannot drift from the bytecode above it.
	Reply json.RawMessage `json:"reply"`
	// Limits are consensus ceilings a caller has to batch under, not advice.
	MaxReplyBytes   int `json:"max_reply_bytes"`
	MaxPayloadBytes int `json:"max_payload_bytes"`
}

var (
	lensOnce sync.Once
	lensOut  map[string]*lensArtifact
	lensErr  error
)

// abiEntry is the part of an ABI item this needs: a constructor's inputs, or a
// named method's outputs.
type abiEntry struct {
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Inputs  json.RawMessage `json:"inputs"`
	Outputs json.RawMessage `json:"outputs"`
}

func buildLensArtifacts() (map[string]*lensArtifact, error) {
	out := map[string]*lensArtifact{}
	for key, pair := range map[string][2]string{
		"asset": {"AssetLens", "IAssetLens"},
		"price": {"PriceLens", "IPriceLens"},
	} {
		impl, err := contracts.Load(pair[0])
		if err != nil {
			return nil, err
		}
		iface, err := contracts.Load(pair[1])
		if err != nil {
			return nil, err
		}

		var implABI, ifaceABI []abiEntry
		if err := json.Unmarshal(impl.ABI, &implABI); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(iface.ABI, &ifaceABI); err != nil {
			return nil, err
		}

		a := &lensArtifact{
			Name:            pair[0],
			Creation:        impl.Bytecode,
			MaxReplyBytes:   24576, // EIP-170
			MaxPayloadBytes: 49152, // EIP-3860
		}
		for _, e := range implABI {
			if e.Type == "constructor" {
				a.Request = e.Inputs
			}
		}
		for _, e := range ifaceABI {
			if e.Type == "function" && e.Name == "query" {
				a.Reply = e.Outputs
			}
		}
		if a.Request == nil || a.Reply == nil {
			// Better to serve nothing than a half-described calling convention that a
			// client discovers is wrong only when a decode returns nonsense.
			continue
		}
		out[key] = a
	}
	return out, nil
}

// listLenses serves GET /v1/lens, the whole set.
func (s *Server) listLenses(w http.ResponseWriter, r *http.Request) {
	lensOnce.Do(func() { lensOut, lensErr = buildLensArtifacts() })
	if lensErr != nil {
		writeErr(w, http.StatusInternalServerError, "lens artifacts unavailable", lensErr)
		return
	}
	// Immutable for the life of a build, and a client fetches it before every batch
	// of calls, so let it be cached hard.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{
		"lenses": lensOut,
		"how": "eth_call({data: creation || abi.encode(request)}, \"latest\") with no `to` address; " +
			"decode the result as `reply`. Batch tokens so the reply stays under max_reply_bytes.",
	})
}
