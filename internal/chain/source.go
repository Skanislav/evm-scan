// Package chain is the only place the indexer touches an execution client.
//
// Everything above this package is written against Source, so the same indexer runs
// against our own snap-synced geth over IPC (the production posture) or against any
// JSON-RPC endpoint (development convenience) without code changes.
//
// A snap-synced node is deliberately sufficient for this design. Snap sync backfills
// every header, body and receipt to genesis while keeping state only at the head, so
// it can serve:
//
//	eth_getLogs over all history  -> discovery backfill
//	eth_call at the head          -> current balances
//
// It cannot serve historical state (no archive), so nothing here may ask for a balance
// at an old block. That constraint is what keeps the node requirement at snap sync
// rather than a multi-terabyte archive node.
package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// ErrNotStreaming is returned by SubscribeLogs on a transport without eth_subscribe.
var ErrNotStreaming = errors.New("chain: endpoint does not support subscriptions")

// Query selects logs over an inclusive block range.
type Query struct {
	From      uint64
	To        uint64
	Addresses []common.Address
	Topics    [][]common.Hash
}

func (q Query) filter() ethereum.FilterQuery {
	return ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(q.From),
		ToBlock:   new(big.Int).SetUint64(q.To),
		Addresses: q.Addresses,
		Topics:    q.Topics,
	}
}

// Source is the read surface of an execution client.
type Source interface {
	ChainID(ctx context.Context) (uint64, error)
	HeadBlock(ctx context.Context) (uint64, error)
	// HeaderHash returns the canonical hash at a height, used to detect reorgs.
	HeaderHash(ctx context.Context, number uint64) (common.Hash, error)
	Logs(ctx context.Context, q Query) ([]types.Log, error)
	SubscribeLogs(ctx context.Context, q Query, out chan<- types.Log) (ethereum.Subscription, error)
	// CallAtHead executes a read-only call against head state. Head only: a snap-synced
	// node has no historical state.
	CallAtHead(ctx context.Context, msg ethereum.CallMsg) ([]byte, error)
	CodeAt(ctx context.Context, addr common.Address) ([]byte, error)
	// NonceAt returns the account's transaction count at head. It is here because
	// the EVM cannot see it: there is no opcode for another account's nonce, so no
	// contract call can report one and it has to come off the RPC directly.
	NonceAt(ctx context.Context, addr common.Address) (uint64, error)
	Endpoint() Endpoint
	Close()
}

// Node is a Source backed by a go-ethereum execution client.
type Node struct {
	ep  Endpoint
	rpc *rpc.Client
	eth *ethclient.Client
}

// Dial connects to a node. When requireLocal is set, a non-loopback endpoint is
// rejected outright rather than silently turning the deployment into a third-party
// dependency.
func Dial(ctx context.Context, raw string, requireLocal bool) (*Node, error) {
	ep, err := ParseEndpoint(raw)
	if err != nil {
		return nil, err
	}
	if requireLocal && !ep.Local {
		return nil, fmt.Errorf(
			"chain: endpoint %s is not local and require_local_node is set; "+
				"point this at your own geth (ipc path, ws://127.0.0.1:… or http://127.0.0.1:…) "+
				"or set require_local_node: false to opt into a third-party RPC",
			ep)
	}

	c, err := rpc.DialContext(ctx, ep.Raw)
	if err != nil {
		return nil, fmt.Errorf("chain: dial %s: %w", ep, err)
	}
	return &Node{ep: ep, rpc: c, eth: ethclient.NewClient(c)}, nil
}

func (n *Node) Endpoint() Endpoint { return n.ep }

func (n *Node) Close() { n.rpc.Close() }

func (n *Node) ChainID(ctx context.Context) (uint64, error) {
	id, err := n.eth.ChainID(ctx)
	if err != nil {
		return 0, err
	}
	if !id.IsUint64() {
		return 0, fmt.Errorf("chain: chain id %s does not fit uint64", id)
	}
	return id.Uint64(), nil
}

func (n *Node) HeadBlock(ctx context.Context) (uint64, error) {
	return n.eth.BlockNumber(ctx)
}

func (n *Node) HeaderHash(ctx context.Context, number uint64) (common.Hash, error) {
	h, err := n.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
	if err != nil {
		return common.Hash{}, err
	}
	return h.Hash(), nil
}

func (n *Node) Logs(ctx context.Context, q Query) ([]types.Log, error) {
	return n.eth.FilterLogs(ctx, q.filter())
}

func (n *Node) SubscribeLogs(ctx context.Context, q Query, out chan<- types.Log) (ethereum.Subscription, error) {
	if !n.ep.Streaming {
		return nil, ErrNotStreaming
	}
	// A subscription is open-ended; From/To are meaningless to eth_subscribe.
	f := ethereum.FilterQuery{Addresses: q.Addresses, Topics: q.Topics}
	return n.eth.SubscribeFilterLogs(ctx, f, out)
}

func (n *Node) CallAtHead(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	// nil blockNumber == "latest".
	return n.eth.CallContract(ctx, msg, nil)
}

func (n *Node) CodeAt(ctx context.Context, addr common.Address) ([]byte, error) {
	return n.eth.CodeAt(ctx, addr, nil)
}

func (n *Node) NonceAt(ctx context.Context, addr common.Address) (uint64, error) {
	return n.eth.NonceAt(ctx, addr, nil)
}

// --------------------------------------------------------------------------
// Adaptive range scanning
// --------------------------------------------------------------------------

// ChunkOpts bounds an adaptive eth_getLogs sweep.
type ChunkOpts struct {
	// Max is the starting window size in blocks.
	Max uint64
	// Min is the floor below which we stop halving and surface the error.
	Min uint64
}

func (o ChunkOpts) withDefaults() ChunkOpts {
	if o.Max == 0 {
		o.Max = 10_000
	}
	if o.Min == 0 {
		o.Min = 1
	}
	if o.Min > o.Max {
		o.Min = o.Max
	}
	return o
}

// ChunkFunc receives each window's logs in ascending block order. Returning an error
// aborts the sweep.
type ChunkFunc func(ctx context.Context, from, to uint64, logs []types.Log) error

// SweepLogs walks [from, to] in windows, halving on provider-side range/result limits.
//
// Our own node has no such limits, but the window still bounds memory for hot
// contracts, and the halving keeps the same code path working against a third-party
// RPC in development.
func SweepLogs(ctx context.Context, src Source, q Query, opts ChunkOpts, fn ChunkFunc) error {
	opts = opts.withDefaults()
	if q.To < q.From {
		return nil
	}

	window := opts.Max
	for cursor := q.From; cursor <= q.To; {
		end := cursor + window - 1
		if end > q.To {
			end = q.To
		}

		sub := q
		sub.From, sub.To = cursor, end
		logs, err := src.Logs(ctx, sub)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if window > opts.Min && isRangeLimit(err) {
				window = max(opts.Min, window/2)
				continue
			}
			return fmt.Errorf("getLogs [%d,%d]: %w", cursor, end, err)
		}

		if err := fn(ctx, cursor, end, logs); err != nil {
			return err
		}

		cursor = end + 1
		// Creep back up so one hot window does not permanently slow the sweep.
		if window < opts.Max {
			window = min(opts.Max, window*2)
		}
	}
	return nil
}

// isRangeLimit matches the (unstandardised) errors providers return when a getLogs
// window is too wide or matches too many results.
func isRangeLimit(err error) bool {
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"more than", "query returned more than", "limit exceeded", "response size",
		"too many results", "block range", "range is too large", "query timeout",
		"exceed maximum", "-32005",
		// A capped response, which is the same thing as a window that is too wide:
		// halving fixes it, giving up does not. Ankr says the first ("Exceeded max
		// limit of 10485760"), Helios the second when its own jsonrpsee server
		// cannot serialise what it just verified. Measured on mainnet: 20 blocks of
		// discovery topics is 6-9 MB and 50 blocks is over the limit.
		"response is too big", "memory capacity exceeded",
		// Helios's wrapper for an upstream request that did not come back. It is
		// ambiguous — a dead RPC looks the same — but every instance we have seen
		// was an oversized request, and halving a genuinely failing endpoint only
		// costs a few smaller retries before the sweep gives up anyway.
		"error sending request for url",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
