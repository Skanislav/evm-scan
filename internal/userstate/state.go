package userstate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

const MaxEntries = 2000
const MaxRequestBytes = 256 << 10

// Decimal strings preserve uint64 values in JavaScript and JSON.
type Entry struct {
	Kind    string         `json:"kind"`
	ChainID string         `json:"chain_id"`
	Address common.Address `json:"address"`
	Weight  int8           `json:"weight,omitempty"`
}
type Snapshot struct {
	Version   int            `json:"version"`
	Account   common.Address `json:"account"`
	StateRoot common.Hash    `json:"state_root"`
	Revision  string         `json:"revision"`
	Previous  common.Hash    `json:"previous"`
	Deadline  string         `json:"deadline"`
	Entries   []Entry        `json:"entries"`
	Signature hexutil.Bytes  `json:"signature"`
}

func Decimal(s string, bits int) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 || v.BitLen() > bits || v.String() != s {
		return nil, errors.New("non-canonical unsigned decimal")
	}
	return v, nil
}
func (e Entry) Pair() (Pair, error) {
	c, err := Decimal(e.ChainID, 63)
	if err != nil || c.Sign() == 0 {
		return Pair{}, errors.New("chain_id must be a positive int64 decimal string")
	}
	ns := byte(0)
	v := byte(1)
	switch e.Kind {
	case "verdict":
		ns = 1
		if e.Weight != 1 && e.Weight != -1 {
			return Pair{}, errors.New("verdict weight must be +1 or -1")
		}
		v = byte(e.Weight)
	case "asset":
		ns = 2
		if e.Weight != 0 {
			return Pair{}, errors.New("asset must have no weight")
		}
	default:
		return Pair{}, errors.New("unknown entry kind")
	}
	raw := make([]byte, 29)
	raw[0] = ns
	binary.BigEndian.PutUint64(raw[1:9], c.Uint64())
	copy(raw[9:], e.Address[:])
	return Pair{Key: crypto.Keccak256Hash(raw), Value: []byte{v}}, nil
}
func Build(entries []Entry) (*Trie, error) {
	if len(entries) > MaxEntries {
		return nil, errors.New("too many entries")
	}
	counts := map[string]int{}
	assets := 0
	p := make([]Pair, 0, len(entries))
	for _, e := range entries {
		v, err := e.Pair()
		if err != nil {
			return nil, err
		}
		p = append(p, v)
		if e.Kind == "asset" {
			assets++
		} else {
			counts[e.ChainID]++
			if counts[e.ChainID] > 200 {
				return nil, errors.New("at most 200 verdicts per chain")
			}
		}
	}
	if assets > 200 {
		return nil, errors.New("at most 200 remembered assets")
	}
	return NewTrie(p)
}
func (s Snapshot) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{"EIP712Domain": {{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, "State": {{Name: "account", Type: "address"}, {Name: "stateRoot", Type: "bytes32"}, {Name: "revision", Type: "uint64"}, {Name: "previous", Type: "bytes32"}, {Name: "deadline", Type: "uint256"}}}, PrimaryType: "State", Domain: apitypes.TypedDataDomain{Name: "evm-scan state", Version: "1"}, Message: apitypes.TypedDataMessage{"account": s.Account.Hex(), "stateRoot": s.StateRoot.Hex(), "revision": s.Revision, "previous": s.Previous.Hex(), "deadline": s.Deadline}}
}
func (s Snapshot) ID() (common.Hash, error) {
	h, _, err := apitypes.TypedDataAndHash(s.TypedData())
	return common.BytesToHash(h), err
}

// Validate authenticates history without treating the admission deadline as an expiry of state.
func (s Snapshot) Validate() (common.Hash, *Trie, error) {
	if s.Version != 1 {
		return common.Hash{}, nil, errors.New("unsupported state version")
	}
	r, err := Decimal(s.Revision, 64)
	if err != nil || r.Sign() == 0 {
		return common.Hash{}, nil, errors.New("revision must be a positive uint64 string")
	}
	if (r.Uint64() == 1) != (s.Previous == (common.Hash{})) {
		return common.Hash{}, nil, errors.New("only revision 1 has a zero previous identifier")
	}
	if _, err = Decimal(s.Deadline, 256); err != nil {
		return common.Hash{}, nil, err
	}
	t, err := Build(s.Entries)
	if err != nil {
		return common.Hash{}, nil, err
	}
	if t.Root != s.StateRoot {
		return common.Hash{}, nil, errors.New("state root mismatch")
	}
	id, err := s.ID()
	if err != nil {
		return id, nil, err
	}
	sig := append([]byte(nil), s.Signature...)
	if len(sig) != 65 {
		return id, nil, errors.New("signature must be 65 bytes")
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	if sig[64] > 1 || !crypto.ValidateSignatureValues(sig[64], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64]), true) {
		return id, nil, errors.New("non-canonical signature")
	}
	pub, err := crypto.SigToPub(id[:], sig)
	if err != nil || crypto.PubkeyToAddress(*pub) != s.Account {
		return id, nil, errors.New("signature does not authorize account")
	}
	return id, t, nil
}
func Successor(previous Snapshot, next Snapshot) bool {
	n, err := strconv.ParseUint(previous.Revision, 10, 64)
	if err != nil || n == math.MaxUint64 {
		return false
	}
	id, err := previous.ID()
	return err == nil && next.Account == previous.Account && next.Previous == id && next.Revision == strconv.FormatUint(n+1, 10)
}
func AccountKey(a common.Address) common.Hash {
	return crypto.Keccak256Hash([]byte("evmscan/accounts/v1"), a[:])
}

type AccountRevision struct {
	Account common.Address `json:"account"`
	ID      common.Hash    `json:"id"`
}
type Checkpoint struct {
	Version   int               `json:"version"`
	Root      common.Hash       `json:"root"`
	Accounts  []AccountRevision `json:"accounts"`
	Snapshots []Snapshot        `json:"snapshots,omitempty"`
}

func Aggregate(accounts []AccountRevision) (*Trie, error) {
	p := make([]Pair, 0, len(accounts))
	for _, a := range accounts {
		p = append(p, Pair{AccountKey(a.Account), a.ID.Bytes()})
	}
	return NewTrie(p)
}
func (c Checkpoint) Validate() error {
	if c.Version != 1 {
		return errors.New("unsupported checkpoint version")
	}
	t, err := Aggregate(c.Accounts)
	if err != nil {
		return err
	}
	if t.Root != c.Root {
		return errors.New("aggregate root mismatch")
	}
	if len(c.Snapshots) != len(c.Accounts) {
		return errors.New("checkpoint export must include every snapshot")
	}
	m := map[common.Address]common.Hash{}
	for _, s := range c.Snapshots {
		id, _, err := s.Validate()
		if err != nil {
			return err
		}
		if _, ok := m[s.Account]; ok {
			return errors.New("duplicate snapshot account")
		}
		m[s.Account] = id
	}
	for _, a := range c.Accounts {
		if m[a.Account] != a.ID {
			return errors.New("checkpoint snapshot mismatch")
		}
	}
	return nil
}
func Record(kind string, h common.Hash) string { return "evmscan:" + kind + ":1:" + h.Hex() }
func ParseRecord(kind, s string) (common.Hash, error) {
	prefix := "evmscan:" + kind + ":1:"
	if !strings.HasPrefix(s, prefix) {
		return common.Hash{}, errors.New("unsupported ENS state record")
	}
	b, err := hexutil.Decode(strings.TrimPrefix(s, prefix))
	if err != nil || len(b) != 32 {
		return common.Hash{}, fmt.Errorf("invalid record hash")
	}
	return common.BytesToHash(b), nil
}
