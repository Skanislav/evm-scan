// Package hintreg is the bridge between the on-chain HintRegistry and the local index.
//
// It runs in both directions:
//
//	Mirror    — pulls permissionlessly registered assets into the scan set.
//	Publisher — pushes merkle commitments over the derived index back on-chain.
package hintreg

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
)

// RegisteredAsset mirrors HintRegistry.Asset.
type RegisteredAsset struct {
	ChainID      uint64
	Token        common.Address
	Kind         uint8
	FromBlock    uint64
	Registrant   common.Address
	RegisteredAt uint64
	Bond         *big.Int
	Active       bool
}

// abiAsset matches the ABI tuple field-for-field for decoding.
type abiAsset struct {
	ChainId      uint64
	Token        common.Address
	Kind         uint8
	FromBlock    uint64
	Registrant   common.Address
	RegisteredAt uint64
	Bond         *big.Int
	Active       bool
}

// Client reads the registry contract.
type Client struct {
	src  chain.Source
	abi  abi.ABI
	addr common.Address
}

// NewClient binds to a deployed HintRegistry.
func NewClient(src chain.Source, addr common.Address) (*Client, error) {
	parsed, err := contracts.HintRegistryABI()
	if err != nil {
		return nil, err
	}
	return &Client{src: src, abi: parsed, addr: addr}, nil
}

// Address returns the registry's address.
func (c *Client) Address() common.Address { return c.addr }

// ABI exposes the parsed ABI for callers that need to encode their own calls.
func (c *Client) ABI() abi.ABI { return c.abi }

func (c *Client) call(ctx context.Context, method string, args ...any) ([]any, error) {
	in, err := c.abi.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("hintreg: pack %s: %w", method, err)
	}
	out, err := c.src.CallAtHead(ctx, ethereum.CallMsg{To: &c.addr, Data: in})
	if err != nil {
		return nil, fmt.Errorf("hintreg: call %s: %w", method, err)
	}
	vals, err := c.abi.Unpack(method, out)
	if err != nil {
		return nil, fmt.Errorf("hintreg: unpack %s: %w", method, err)
	}
	return vals, nil
}

// AssetCount returns how many assets have ever been registered.
func (c *Client) AssetCount(ctx context.Context) (uint64, error) {
	vals, err := c.call(ctx, "assetCount")
	if err != nil {
		return 0, err
	}
	n, ok := vals[0].(*big.Int)
	if !ok || !n.IsUint64() {
		return 0, fmt.Errorf("hintreg: unexpected assetCount return")
	}
	return n.Uint64(), nil
}

// ListAssets pages through the registry.
//
// This reads current state rather than replaying AssetRegistered logs: the registry is
// small, and a state read cannot drift out of sync with revocations the way a log
// replay can.
func (c *Client) ListAssets(ctx context.Context, pageSize uint64) ([]RegisteredAsset, error) {
	total, err := c.AssetCount(ctx)
	if err != nil {
		return nil, err
	}
	if pageSize == 0 {
		pageSize = 200
	}

	var out []RegisteredAsset
	for offset := uint64(0); offset < total; offset += pageSize {
		vals, err := c.call(ctx, "listAssets",
			new(big.Int).SetUint64(offset), new(big.Int).SetUint64(pageSize))
		if err != nil {
			return nil, err
		}
		page := *abi.ConvertType(vals[0], new([]abiAsset)).(*[]abiAsset)
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			out = append(out, RegisteredAsset{
				ChainID:      a.ChainId,
				Token:        a.Token,
				Kind:         a.Kind,
				FromBlock:    a.FromBlock,
				Registrant:   a.Registrant,
				RegisteredAt: a.RegisteredAt,
				Bond:         a.Bond,
				Active:       a.Active,
			})
		}
	}
	return out, nil
}

// AssetBond is the fee registerAsset requires.
func (c *Client) AssetBond(ctx context.Context) (*big.Int, error) {
	vals, err := c.call(ctx, "assetBond")
	if err != nil {
		return nil, err
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("hintreg: unexpected assetBond return")
	}
	return n, nil
}

// PublisherBond is the fee publishIndex requires.
func (c *Client) PublisherBond(ctx context.Context) (*big.Int, error) {
	vals, err := c.call(ctx, "publisherBond")
	if err != nil {
		return nil, err
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("hintreg: unexpected publisherBond return")
	}
	return n, nil
}

// KindToStandard maps a registry kind sentinel to an evmlog.Standard value.
func KindToStandard(kind uint8) uint8 {
	switch kind {
	case 20, 21, 55:
		return kind
	default:
		return 0
	}
}

// Mode describes how a deployment adjudicates disputed commitments, plus the knobs a
// publisher needs before it can post one.
//
// A deployment is in exactly one mode, fixed at construction: either an optimistic
// oracle settles disputes, or a local arbiter key does. Reading it is the first thing
// the publisher does, because the two modes bond in different currencies.
type Mode struct {
	// Oracle is the optimistic oracle, or the zero address in local-arbiter mode.
	Oracle common.Address
	// BondCurrency is the ERC-20 oracle bonds are denominated in. Zero in
	// local-arbiter mode, where bonds are wei.
	BondCurrency common.Address
	// Arbiter settles challenges in local-arbiter mode. Zero in oracle mode.
	Arbiter common.Address
	// PublisherBond is denominated in BondCurrency in oracle mode, wei otherwise.
	PublisherBond *big.Int
	AssetBond     *big.Int
	// ChallengeWindow is the dispute window in seconds; the assertion liveness in
	// oracle mode.
	ChallengeWindow uint64
}

// OracleMode reports whether disputes go to the oracle rather than to a key.
func (m Mode) OracleMode() bool { return m.Oracle != (common.Address{}) }

// String renders the mode the way the deploy and verify tools print it.
func (m Mode) String() string {
	if m.OracleMode() {
		return fmt.Sprintf("optimistic-oracle (oracle %s, bond %s of %s)",
			m.Oracle.Hex(), m.PublisherBond, m.BondCurrency.Hex())
	}
	return fmt.Sprintf("local-arbiter (arbiter %s, bond %s wei)", m.Arbiter.Hex(), m.PublisherBond)
}

// Mode reads the deployment's adjudication configuration.
func (c *Client) Mode(ctx context.Context) (Mode, error) {
	var m Mode
	for _, f := range []struct {
		method string
		into   *common.Address
	}{
		{"oracle", &m.Oracle},
		{"bondCurrency", &m.BondCurrency},
		{"arbiter", &m.Arbiter},
	} {
		vals, err := c.call(ctx, f.method)
		if err != nil {
			return Mode{}, err
		}
		addr, ok := vals[0].(common.Address)
		if !ok {
			return Mode{}, fmt.Errorf("hintreg: unexpected %s return", f.method)
		}
		*f.into = addr
	}

	for _, f := range []struct {
		method string
		into   **big.Int
	}{
		{"assetBond", &m.AssetBond},
		{"publisherBond", &m.PublisherBond},
	} {
		vals, err := c.call(ctx, f.method)
		if err != nil {
			return Mode{}, err
		}
		n, ok := vals[0].(*big.Int)
		if !ok {
			return Mode{}, fmt.Errorf("hintreg: unexpected %s return", f.method)
		}
		*f.into = n
	}

	vals, err := c.call(ctx, "challengeWindow")
	if err != nil {
		return Mode{}, err
	}
	w, ok := vals[0].(*big.Int)
	if !ok || !w.IsUint64() {
		return Mode{}, fmt.Errorf("hintreg: unexpected challengeWindow return")
	}
	m.ChallengeWindow = w.Uint64()
	return m, nil
}

// EpochStatus mirrors HintRegistry.EpochStatus.
type EpochStatus uint8

const (
	EpochNone EpochStatus = iota
	EpochProposed
	EpochChallenged
	EpochFinalized
	EpochRejected
)

func (s EpochStatus) String() string {
	switch s {
	case EpochProposed:
		return "proposed"
	case EpochChallenged:
		return "challenged"
	case EpochFinalized:
		return "finalized"
	case EpochRejected:
		return "rejected"
	default:
		return "none"
	}
}

// OnchainEpoch mirrors HintRegistry.Epoch.
type OnchainEpoch struct {
	ChainID           uint64
	FromBlock         uint64
	ToBlock           uint64
	Root              common.Hash
	URI               string
	Publisher         common.Address
	Challenger        common.Address
	Bond              *big.Int
	PublishedAt       uint64
	ChallengeDeadline uint64
	Status            EpochStatus
	// AssertionID is the oracle assertion backing the commitment; zero in
	// local-arbiter mode.
	AssertionID common.Hash
}

// abiEpoch matches the ABI tuple field-for-field for decoding.
type abiEpoch struct {
	ChainId           uint64
	FromBlock         uint64
	ToBlock           uint64
	Root              [32]byte
	Uri               string
	Publisher         common.Address
	Challenger        common.Address
	Bond              *big.Int
	PublishedAt       uint64
	ChallengeDeadline uint64
	Status            uint8
	AssertionId       [32]byte
}

// GetEpoch reads one commitment's on-chain state.
func (c *Client) GetEpoch(ctx context.Context, epochID int64) (OnchainEpoch, error) {
	vals, err := c.call(ctx, "getEpoch", big.NewInt(epochID))
	if err != nil {
		return OnchainEpoch{}, err
	}
	e := *abi.ConvertType(vals[0], new(abiEpoch)).(*abiEpoch)
	return OnchainEpoch{
		ChainID:           e.ChainId,
		FromBlock:         e.FromBlock,
		ToBlock:           e.ToBlock,
		Root:              e.Root,
		URI:               e.Uri,
		Publisher:         e.Publisher,
		Challenger:        e.Challenger,
		Bond:              e.Bond,
		PublishedAt:       e.PublishedAt,
		ChallengeDeadline: e.ChallengeDeadline,
		Status:            EpochStatus(e.Status),
		AssertionID:       e.AssertionId,
	}, nil
}

// EpochCount returns how many commitments have been published.
func (c *Client) EpochCount(ctx context.Context) (uint64, error) {
	vals, err := c.call(ctx, "epochCount")
	if err != nil {
		return 0, err
	}
	n, ok := vals[0].(*big.Int)
	if !ok || !n.IsUint64() {
		return 0, fmt.Errorf("hintreg: unexpected epochCount return")
	}
	return n.Uint64(), nil
}

// ERC20ABI is the minimal ERC-20 interface compiled alongside the registry. Bond
// currencies are ordinary tokens, so this is all the publisher needs to approve one.
func ERC20ABI() (abi.ABI, error) {
	art, err := contracts.Load("IERC20")
	if err != nil {
		return abi.ABI{}, err
	}
	return art.Parsed()
}

// Allowance reads an ERC-20 allowance, used to check that a publisher has approved the
// registry to move its oracle bond.
func (c *Client) Allowance(ctx context.Context, token, owner, spender common.Address) (*big.Int, error) {
	parsed, err := ERC20ABI()
	if err != nil {
		return nil, err
	}
	in, err := parsed.Pack("allowance", owner, spender)
	if err != nil {
		return nil, fmt.Errorf("hintreg: pack allowance: %w", err)
	}
	out, err := c.src.CallAtHead(ctx, ethereum.CallMsg{To: &token, Data: in})
	if err != nil {
		return nil, fmt.Errorf("hintreg: call allowance: %w", err)
	}
	vals, err := parsed.Unpack("allowance", out)
	if err != nil {
		return nil, fmt.Errorf("hintreg: unpack allowance: %w", err)
	}
	n, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("hintreg: unexpected allowance return")
	}
	return n, nil
}

// Simulate runs a state-changing call as `from` without sending it, so a caller can
// tell whether it would revert. The registry's finalize path reverts while a window is
// still open or a dispute is still being voted on, both of which are normal states to
// poll through rather than pay gas to discover.
func (c *Client) Simulate(ctx context.Context, from common.Address, method string, args ...any) error {
	in, err := c.abi.Pack(method, args...)
	if err != nil {
		return fmt.Errorf("hintreg: pack %s: %w", method, err)
	}
	_, err = c.src.CallAtHead(ctx, ethereum.CallMsg{From: from, To: &c.addr, Data: in})
	return err
}
