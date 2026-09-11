// Package ccip is the ERC-3668 (CCIP Read) side of HintRegistry.contractsOf.
//
// The contract answers "which contracts has this account touched?" by reverting
// with OffchainLookup: the client fetches the account's leaf and proof from a
// gateway, then calls back into the contract, which verifies them against the
// committed root. This package holds the pieces both ends share: the response
// encoding a gateway produces and a callback consumes, and a small client that runs
// the revert-fetch-callback dance against any node.
//
// It talks to a node only through a caller the user hands in, so internal/chain
// stays the one package that dials one.
package ccip

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Method and callback names in HintRegistry.
const (
	MethodContractsOf = "contractsOf"
	MethodCallback    = "contractsOfCallback"
	ErrorName         = "OffchainLookup"
)

var responseArgs = mustArgs("uint256", "address[]", "bytes32[]")

func mustArgs(types ...string) abi.Arguments {
	out := make(abi.Arguments, len(types))
	for i, t := range types {
		ty, err := abi.NewType(t, "", nil)
		if err != nil {
			panic(err)
		}
		out[i] = abi.Argument{Type: ty}
	}
	return out
}

// SortedUnique returns the ascending, de-duplicated form the contract's assetsHash
// insists on.
func SortedUnique(assets []common.Address) []common.Address {
	out := make([]common.Address, 0, len(assets))
	out = append(out, assets...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	n := 0
	for i := range out {
		if i > 0 && out[i] == out[i-1] {
			continue
		}
		out[n] = out[i]
		n++
	}
	return out[:n]
}

// EncodeResponse builds what a gateway returns for contractsOf:
// abi.encode(epochId, assets, proof).
func EncodeResponse(epochID int64, assets []common.Address, proof []common.Hash) ([]byte, error) {
	p := make([][32]byte, len(proof))
	for i, h := range proof {
		p[i] = h
	}
	return responseArgs.Pack(new(big.Int).SetInt64(epochID), SortedUnique(assets), p)
}

// DecodeResponse is the inverse of EncodeResponse.
func DecodeResponse(b []byte) (epochID int64, assets []common.Address, proof []common.Hash, err error) {
	vals, err := responseArgs.Unpack(b)
	if err != nil {
		return 0, nil, nil, err
	}
	id, ok := vals[0].(*big.Int)
	if !ok || !id.IsInt64() {
		return 0, nil, nil, errors.New("ccip: bad epoch id")
	}
	assets, _ = vals[1].([]common.Address)
	raw, _ := vals[2].([][32]byte)
	proof = make([]common.Hash, len(raw))
	for i, h := range raw {
		proof[i] = h
	}
	return id.Int64(), assets, proof, nil
}

// Lookup is a decoded OffchainLookup revert.
type Lookup struct {
	Sender    common.Address
	URLs      []string
	CallData  []byte
	Callback  [4]byte
	ExtraData []byte
}

// OffchainLookup's shape is fixed by ERC-3668, so it can be decoded without any
// contract's ABI. The selector is derived from the signature, never pasted.
var (
	offchainLookupArgs     = mustArgs("address", "string[]", "bytes", "bytes4", "bytes")
	OffchainLookupSelector = [4]byte(crypto.Keccak256([]byte("OffchainLookup(address,string[],bytes,bytes4,bytes)"))[:4])
)

// ParseOffchainLookup decodes revert data as ERC-3668 OffchainLookup from any
// contract. ok is false when the data is some other error.
func ParseOffchainLookup(revertData []byte) (*Lookup, bool, error) {
	if len(revertData) < 4 || !bytes.Equal(revertData[:4], OffchainLookupSelector[:]) {
		return nil, false, nil
	}
	vals, err := offchainLookupArgs.Unpack(revertData[4:])
	if err != nil {
		return nil, false, fmt.Errorf("ccip: decode OffchainLookup: %w", err)
	}
	l := &Lookup{}
	l.Sender, _ = vals[0].(common.Address)
	l.URLs, _ = vals[1].([]string)
	l.CallData, _ = vals[2].([]byte)
	l.Callback, _ = vals[3].([4]byte)
	l.ExtraData, _ = vals[4].([]byte)
	return l, true, nil
}

// ParseLookup decodes revert data as OffchainLookup, checking the selector against
// the contract's own ABI. ok is false when the data is some other error.
func ParseLookup(contractABI abi.ABI, revertData []byte) (*Lookup, bool, error) {
	ev, found := contractABI.Errors[ErrorName]
	if !found {
		return nil, false, errors.New("ccip: ABI has no OffchainLookup error")
	}
	if len(revertData) < 4 || !bytes.Equal(revertData[:4], ev.ID[:4]) {
		return nil, false, nil
	}
	return ParseOffchainLookup(revertData)
}

// RevertData pulls the raw revert bytes out of an eth_call error, which both
// go-ethereum's RPC client and its simulated backend expose through ErrorData.
func RevertData(err error) ([]byte, bool) {
	var de interface{ ErrorData() interface{} }
	if !errors.As(err, &de) {
		return nil, false
	}
	s, ok := de.ErrorData().(string)
	if !ok {
		return nil, false
	}
	b, err := hexutil.Decode(s)
	if err != nil {
		return nil, false
	}
	return b, true
}

// Caller executes a read-only call at head and returns the raw result. On revert
// it must return an error RevertData can read.
type Caller func(ctx context.Context, to common.Address, data []byte) ([]byte, error)

// Fetch asks one gateway for the lookup's answer. A template containing {data}
// is queried with GET, anything else with POST, as ERC-3668 specifies.
func Fetch(ctx context.Context, hc *http.Client, template string, sender common.Address, callData []byte) ([]byte, error) {
	senderHex := strings.ToLower(sender.Hex())
	dataHex := hexutil.Encode(callData)
	var (
		req *http.Request
		err error
	)
	if strings.Contains(template, "{data}") {
		u := strings.NewReplacer("{sender}", senderHex, "{data}", dataHex).Replace(template)
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	} else {
		u := strings.ReplaceAll(template, "{sender}", senderHex)
		body, _ := json.Marshal(map[string]string{"sender": senderHex, "data": dataHex})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ccip: gateway %s: %s: %s", template, resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ccip: gateway response is not JSON: %w", err)
	}
	b, err := hexutil.Decode(out.Data)
	if err != nil {
		return nil, fmt.Errorf("ccip: gateway data is not hex: %w", err)
	}
	return b, nil
}

// Resolve runs the full ERC-3668 flow for one call: execute it, decode the
// OffchainLookup it reverts with, fetch from the first gateway that answers
// (override with urls, which win over the contract's list when non-empty), then
// call back and return the callback's raw result. A call that does not revert is
// returned as is.
//
// The callback is looked up in contractABI by the selector the revert named, so the
// same client serves any ERC-3668 contract whose callback takes
// (bytes response, bytes extraData): HintRegistry.contractsOfCallback and
// HintResolver.resolveCallback alike.
func Resolve(ctx context.Context, call Caller, contractABI abi.ABI, to common.Address, callData []byte, hc *http.Client, urls []string) ([]byte, error) {
	out, err := call(ctx, to, callData)
	if err == nil {
		return out, nil
	}
	revert, ok := RevertData(err)
	if !ok {
		return nil, err
	}
	lookup, ok, perr := ParseLookup(contractABI, revert)
	if perr != nil {
		return nil, perr
	}
	if !ok {
		return nil, fmt.Errorf("ccip: call reverted: %w", err)
	}
	if lookup.Sender != to {
		return nil, fmt.Errorf("ccip: lookup sender %s is not the callee %s", lookup.Sender.Hex(), to.Hex())
	}
	method, err := contractABI.MethodById(lookup.Callback[:])
	if err != nil {
		return nil, fmt.Errorf("ccip: contract asked for callback %x, which the ABI does not have", lookup.Callback)
	}
	if len(urls) == 0 {
		urls = lookup.URLs
	}
	if len(urls) == 0 {
		return nil, errors.New("ccip: contract advertises no gateways; pass one explicitly")
	}

	var errs []error
	for _, u := range urls {
		resp, ferr := Fetch(ctx, hc, u, lookup.Sender, lookup.CallData)
		if ferr != nil {
			errs = append(errs, ferr)
			continue
		}
		cb, err := contractABI.Pack(method.Name, resp, lookup.ExtraData)
		if err != nil {
			return nil, fmt.Errorf("ccip: pack callback %s: %w", method.Name, err)
		}
		return call(ctx, to, cb)
	}
	return nil, fmt.Errorf("ccip: no gateway answered: %w", errors.Join(errs...))
}

// ContractsOfCallData packs the lookup for an account.
func ContractsOfCallData(regABI abi.ABI, chainID uint64, account common.Address) ([]byte, error) {
	return regABI.Pack(MethodContractsOf, chainID, account)
}

// DecodeContractsOf reads the callback's address list.
func DecodeContractsOf(regABI abi.ABI, out []byte) ([]common.Address, error) {
	vals, err := regABI.Unpack(MethodCallback, out)
	if err != nil {
		return nil, err
	}
	assets, _ := vals[0].([]common.Address)
	return assets, nil
}

// Selector renders four bytes for logs and errors.
func Selector(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	return "0x" + hex.EncodeToString(b[:4])
}
