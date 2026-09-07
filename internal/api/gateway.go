package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/store"
)

// --------------------------------------------------------------------------
// ERC-3668 gateway for HintRegistry.contractsOf
// --------------------------------------------------------------------------
//
// The registry reverts with OffchainLookup pointing here. We answer with the
// account's leaf and proof for the latest finalized epoch, abi-encoded the way
// contractsOfCallback expects. The contract verifies everything we return, so this
// endpoint can be run by anyone with an index and trusted by no one.

// ccipGet serves GET /ccip/{sender}/{data}.json.
func (s *Server) ccipGet(w http.ResponseWriter, r *http.Request) {
	s.serveCCIP(w, r, r.PathValue("sender"), strings.TrimSuffix(r.PathValue("data"), ".json"))
}

// ccipPost serves POST /ccip with {"sender": "0x…", "data": "0x…"}.
func (s *Server) ccipPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sender string `json:"sender"`
		Data   string `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body", err)
		return
	}
	s.serveCCIP(w, r, req.Sender, req.Data)
}

func (s *Server) serveCCIP(w http.ResponseWriter, r *http.Request, senderHex, dataHex string) {
	if s.d.Registry == nil {
		writeErr(w, http.StatusServiceUnavailable, "no registry configured", nil)
		return
	}
	sender, err := parseAddress(senderHex)
	if err != nil || sender != s.d.Registry.Address() {
		writeErr(w, http.StatusBadRequest, "sender is not this gateway's registry", err)
		return
	}
	data, err := hexutil.Decode(dataHex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad calldata", err)
		return
	}

	regABI := s.d.Registry.ABI()
	method, ok := regABI.Methods[ccip.MethodContractsOf]
	if !ok {
		writeErr(w, http.StatusInternalServerError, "ABI has no contractsOf", nil)
		return
	}
	if len(data) < 4 || !bytes.Equal(data[:4], method.ID) {
		writeErr(w, http.StatusBadRequest, "unsupported selector "+ccip.Selector(data), nil)
		return
	}
	vals, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad contractsOf arguments", err)
		return
	}
	chainID, _ := vals[0].(uint64)
	account, _ := vals[1].(common.Address)

	ctx := r.Context()
	e, err := s.latestFinalizedFor(ctx, chainID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "this indexer has not published the latest finalized epoch", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "registry unreachable", err)
		return
	}

	pr, err := s.accountProof(ctx, e, account)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "account not in the latest finalized commitment", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "proof failed", err)
		return
	}

	resp, err := ccip.EncodeResponse(*e.OnchainID, pr.assets, pr.proof)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"data": hexutil.Encode(resp)})
}

// latestFinalizedFor resolves the epoch contractsOfCallback will accept: the
// registry's latest finalized id, mapped to our local commitment. A local view
// alone could lag the chain and hand out proofs the contract then rejects.
func (s *Server) latestFinalizedFor(ctx context.Context, chainID uint64) (store.Epoch, error) {
	found, id, err := s.d.Registry.LatestFinalizedEpoch(ctx, chainID)
	if err != nil {
		return store.Epoch{}, err
	}
	if !found {
		return store.Epoch{}, store.ErrNotFound
	}
	return s.d.Store.EpochByOnchainID(ctx, chainID, id)
}

// accountProof is one account's inclusion proof in an epoch, with the asset list
// that hashes to its leaf.
type accountProof struct {
	leaf   store.EpochLeaf
	proof  []common.Hash
	assets []common.Address
}

// accountProof rebuilds the proof and returns the asset list the leaf was committed
// over. That list is stored with the leaf, because it cannot be re-derived: a
// backfill walking an asset toward its floor keeps inserting interactions whose
// first_block lies inside a range already committed, so the live rollup grows a
// superset of what was hashed and the contract would reject it.
//
// Epochs built before migration 0005 have no stored list. For those the rollup is
// the only source, and the hash is checked against the leaf so a drifted answer is
// an error rather than one the contract refuses.
func (s *Server) accountProof(ctx context.Context, e store.Epoch, account common.Address) (accountProof, error) {
	leaf, proof, err := hintreg.ProofFor(ctx, s.d.Store, e.ID, account)
	if err != nil {
		return accountProof{}, err
	}
	assets := leaf.Assets
	if assets == nil {
		rows, err := s.d.Store.AccountAssets(ctx, e.ChainID, account)
		if err != nil {
			return accountProof{}, err
		}
		assets = make([]common.Address, 0, len(rows))
		for _, a := range rows {
			if a.FirstBlock <= e.ToBlock {
				assets = append(assets, a.Asset)
			}
		}
		assets = ccip.SortedUnique(assets)
	}
	if merkle.AssetsHash(assets) != leaf.AssetsHash {
		return accountProof{}, fmt.Errorf("asset list for epoch %d does not hash to the committed leaf", e.ID)
	}
	return accountProof{leaf: leaf, proof: proof, assets: assets}, nil
}
