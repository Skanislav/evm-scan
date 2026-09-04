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
