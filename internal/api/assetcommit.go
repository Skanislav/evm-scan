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

type assetCommitItemJSON struct {
	ChainID uint64 `json:"chain_id"`
	Address string `json:"address"`
}

type assetCommitRequest struct {
	Assets    []assetCommitItemJSON `json:"assets"`
	Deadline  string                `json:"deadline"`
	Signature string                `json:"signature"`
}

func assetCommitView(c store.AssetCommit) map[string]any {
	assets := make([]assetCommitItemJSON, len(c.Items))
	for i, item := range c.Items {
		assets[i] = assetCommitItemJSON{ChainID: item.ChainID, Address: item.Asset.Hex()}
	}
	return map[string]any{
		"account":   c.Account.Hex(),
		"assets":    assets,
		"digest":    c.Digest.Hex(),
		"deadline":  c.Deadline,
		"signed_at": c.SignedAt.UTC().Format(time.RFC3339),
	}
}

// getAssetCommit returns the account's exact signed list. It is intentionally public:
// the account signed this enumerable disclosure so clients can avoid rediscovering the
// same contracts. No commitment means callers must fall back to their normal list.
func (s *Server) getAssetCommit(w http.ResponseWriter, r *http.Request) {
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	c, err := s.d.Store.AssetCommit(r.Context(), account)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no signed asset commit for this account", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read asset commit", err)
		return
	}
	writeJSON(w, http.StatusOK, assetCommitView(c))
}

// postAssetCommit replaces the reader's exact list only after an EIP-712 signature
// proves the account chose to disclose it. It never registers, promotes, or scans any
// asset: the list only saves the reader's future balance calls.
func (s *Server) postAssetCommit(w http.ResponseWriter, r *http.Request) {
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	var req assetCommitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	if len(req.Assets) > store.MaxAssetCommitItems {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("an asset commit names at most %d pairs", store.MaxAssetCommitItems), nil)
		return
	}
	items := make([]store.AssetCommitItem, 0, len(req.Assets))
	seen := make(map[store.AssetCommitItem]bool, len(req.Assets))
	for _, item := range req.Assets {
		if item.ChainID == 0 || item.ChainID > uint64(1<<63-1) {
			writeErr(w, http.StatusBadRequest, "assets[].chain_id must be a positive int64", nil)
			return
		}
		if !common.IsHexAddress(item.Address) {
			writeErr(w, http.StatusBadRequest, "assets[].address must be an address", nil)
			return
		}
		v := store.AssetCommitItem{ChainID: item.ChainID, Asset: common.HexToAddress(item.Address)}
		if seen[v] {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%d:%s appears twice", v.ChainID, v.Asset.Hex()), nil)
			return
		}
		seen[v] = true
		items = append(items, v)
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
	unixDeadline, err := store.AssetCommitDeadline(deadline)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "deadline is outside the supported unix range", nil)
		return
	}
	sig, err := hexutil.Decode(req.Signature)
	if err != nil || len(sig) != 65 {
		writeErr(w, http.StatusBadRequest, "signature must be 65 bytes of hex", err)
		return
	}
	digest := assetCommitDigest(items)
	who, err := recoverAssetCommitSigner(account, digest, deadline, sig)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad signature", err)
		return
	}
	if who != account {
		writeErr(w, http.StatusUnauthorized, fmt.Sprintf("signed by %s, not by the account", who.Hex()), nil)
		return
	}

	commit := store.AssetCommit{Account: account, Items: items, Digest: digest, Deadline: unixDeadline}
	if err := s.d.Store.PutAssetCommit(r.Context(), commit); errors.Is(err, store.ErrStaleAssetCommit) {
		writeErr(w, http.StatusConflict, "an asset commit with a later deadline is already stored; sign a new one", nil)
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store asset commit", err)
		return
	}
	stored, err := s.d.Store.AssetCommit(r.Context(), account)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read stored asset commit", err)
		return
	}
	writeJSON(w, http.StatusOK, assetCommitView(stored))
}
