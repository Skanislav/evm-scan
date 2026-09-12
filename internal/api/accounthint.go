package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/store"
)

// A reader's own cross-chain hint, and the index's stand-in for one.
//
// GET serves the hint for an account: the bloom the reader signed for when there
// is one, else a bloom the index builds from the account's contracts on every chain
// it runs. Either way it is a KindInterop bloom in the .xorf format, and either way
// it only ever orders a sweep — a miss is "not known when this was built", never
// "not held". POST stores a reader's own, under the reader's signature.
//
// What the store keeps is per-account and unblinded (migrations/0011). The gate is
// the signature: nobody but the account can put a hint under its address, and the
// deadline keeps an old signature from putting an old hint back.

const (
	hintMaxBits  = 4096
	hintMaxBytes = 47 + hintMaxBits/8 // header with no descriptor, plus the bitmap
	hintMinGap   = 10 * time.Minute
)

type hintMeta struct {
	Source     string  `json:"source"` // reader | index
	Count      uint64  `json:"count"`
	Bits       uint32  `json:"m"`
	Probes     uint8   `json:"k"`
	Saturation float64 `json:"saturation"`
	Keccak     string  `json:"keccak256"`
	EpochID    int64   `json:"epoch_id"`
	ToBlock    uint64  `json:"to_block"`
	Deadline   int64   `json:"deadline,omitempty"`
	SignedAt   string  `json:"signed_at,omitempty"`
}

func (s *Server) postAccountHint(w http.ResponseWriter, r *http.Request) {
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	var req struct {
		Bytes     string `json:"bytes"`
		Deadline  string `json:"deadline"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(req.Bytes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bytes must be base64url", err)
		return
	}
	if len(raw) > hintMaxBytes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a hint is at most %d bytes", hintMaxBytes), nil)
		return
	}
	f, err := hintfilter.Decode(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bytes are not a .xorf filter", err)
		return
	}
	m, _ := f.BloomParams()
	switch {
	case f.Structure != hintfilter.StructureBloom:
		writeErr(w, http.StatusBadRequest, "a hint is a bloom filter", nil)
		return
	case f.Kind != hintfilter.KindInterop:
		writeErr(w, http.StatusBadRequest, "a hint is keyed by ERC-7930 pair (KindInterop)", nil)
		return
	case f.ChainID != 0:
		writeErr(w, http.StatusBadRequest, "an interop hint binds no single chain", nil)
		return
	case f.Blinded:
		writeErr(w, http.StatusBadRequest, "a hint is public; a blinded filter answers nobody", nil)
		return
	case m > hintMaxBits:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a hint is at most %d bits", hintMaxBits), nil)
		return
	}
	deadline, ok := new(big.Int).SetString(req.Deadline, 10)
	if !ok || !deadline.IsInt64() {
		writeErr(w, http.StatusBadRequest, "deadline must be a decimal unix time", nil)
		return
	}
	if deadline.Int64() < time.Now().Unix() {
		writeErr(w, http.StatusBadRequest, "the signature has expired", nil)
		return
	}
	sig, err := hexutil.Decode(req.Signature)
	if err != nil || len(sig) != 65 {
		writeErr(w, http.StatusBadRequest, "signature must be 65 bytes of hex", err)
		return
	}
	digest := crypto.Keccak256Hash(raw)
	who, err := recoverHintSigner(account, digest, deadline, sig)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad signature", err)
		return
	}
	if who != account {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("signed by %s, not by the account", who.Hex()), nil)
		return
	}

	ctx := r.Context()
	if prev, err := s.d.Store.AccountHint(ctx, account); err == nil && time.Since(prev.SignedAt) < hintMinGap {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("one hint per %s per account", hintMinGap), nil)
		return
	}
	err = s.d.Store.PutAccountHint(ctx, store.AccountHint{Account: account, Bytes: raw, Digest: digest, Deadline: deadline.Int64()})
	if errors.Is(err, store.ErrStaleHint) {
		writeErr(w, http.StatusConflict, "a hint with a later deadline is already stored", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":    account.Hex(),
		"count":      f.Count(),
		"m":          m,
		"saturation": f.Saturation(),
		"keccak256":  digest.Hex(),
		"deadline":   deadline.Int64(),
	})
}

func (s *Server) getAccountHint(w http.ResponseWriter, r *http.Request) {
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	raw, meta, err := s.accountHintBytes(r.Context(), account)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no hint for this account and nothing indexed to build one from", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not build the hint", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("ETag", `"`+meta.Keccak+`"`)
	w.Header().Set("X-Hint-Source", meta.Source)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = w.Write(raw)
}

func (s *Server) getAccountHintJSON(w http.ResponseWriter, r *http.Request) {
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	_, meta, err := s.accountHintBytes(r.Context(), account)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no hint for this account and nothing indexed to build one from", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not build the hint", err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// accountHintBytes is the hint for an account: the reader's own when stored, else
// one built from what the index committed for the account on every chain it runs.
// The index-built one prefers the latest finalized leaf, so it names the same
// contracts the signed evmscan.contracts record does; a chain with no finalized
// epoch yet contributes its live rows.
func (s *Server) accountHintBytes(ctx context.Context, account common.Address) ([]byte, hintMeta, error) {
	if s.d.Store != nil {
		if h, err := s.d.Store.AccountHint(ctx, account); err == nil {
			f, err := hintfilter.Decode(h.Bytes)
			if err != nil {
				return nil, hintMeta{}, err
			}
			m, k := f.BloomParams()
			return h.Bytes, hintMeta{
				Source: "reader", Count: f.Count(), Bits: m, Probes: k, Saturation: f.Saturation(),
				Keccak: h.Digest.Hex(), EpochID: f.EpochID, ToBlock: f.ToBlock,
				Deadline: h.Deadline, SignedAt: h.SignedAt.UTC().Format(time.RFC3339),
			}, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, hintMeta{}, err
		}
	}

	sub := hintfilter.InteropSubkey(hintfilter.PublicSecret)
	var keys []uint64
	meta := hintfilter.Meta{Structure: hintfilter.StructureBloom, Kind: hintfilter.KindInterop, EpochID: -1}
	for _, e := range s.d.Chains.Entries() {
		assets, epochID, toBlock, err := s.committedAssets(ctx, e.ID, account)
		if err != nil {
			return nil, hintMeta{}, err
		}
		for _, a := range assets {
			keys = append(keys, hintfilter.InteropKey(sub, e.ID, a))
		}
		if meta.EpochID < 0 && epochID >= 0 {
			meta.EpochID, meta.ToBlock = epochID, toBlock
		}
	}
	if len(keys) == 0 {
		return nil, hintMeta{}, store.ErrNotFound
	}
	if bits := hintfilter.BloomBits(len(keys), 0.01); bits > hintfilter.HintBits {
		meta.BloomBits = bits
	} else {
		meta.BloomBits = hintfilter.HintBits
	}
	if meta.BloomBits > hintMaxBits {
		meta.BloomBits = hintMaxBits
	}
	f, err := hintfilter.Build(keys, meta)
	if err != nil {
		return nil, hintMeta{}, err
	}
	raw, err := f.Encode()
	if err != nil {
		return nil, hintMeta{}, err
	}
	m, k := f.BloomParams()
	return raw, hintMeta{
		Source: "index", Count: f.Count(), Bits: m, Probes: k, Saturation: f.Saturation(),
		Keccak: crypto.Keccak256Hash(raw).Hex(), EpochID: f.EpochID, ToBlock: f.ToBlock,
	}, nil
}

// committedAssets is what the index says an account touched on a chain: the latest
// finalized leaf when the registry has one and the account is in it, else the live
// rollup, with epochID -1 to say so.
func (s *Server) committedAssets(ctx context.Context, chainID uint64, account common.Address) ([]common.Address, int64, uint64, error) {
	if s.d.Registry != nil {
		e, err := s.latestFinalizedFor(ctx, chainID)
		if err == nil {
			pr, err := s.accountProof(ctx, e, account)
			if err == nil {
				id := int64(-1)
				if e.OnchainID != nil {
					id = *e.OnchainID
				}
				return pr.assets, id, e.ToBlock, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, -1, 0, err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, -1, 0, err
		}
	}
	if s.d.Store == nil {
		return nil, -1, 0, nil
	}
	rows, err := s.d.Store.AccountAssets(ctx, chainID, account)
	if err != nil {
		return nil, -1, 0, err
	}
	out := make([]common.Address, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.Asset)
	}
	return out, -1, 0, nil
}
