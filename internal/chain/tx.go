package chain

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Sender is the write surface of a node.
//
// It is kept separate from Source so that the indexer, which must never send a
// transaction, cannot: it only ever holds a Source. Only the commitment publisher
// takes a Sender.
type Sender interface {
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

var _ Sender = (*Node)(nil)

func (n *Node) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	n.meter.add("eth_getTransactionCount")
	return n.eth.PendingNonceAt(ctx, account)
}

func (n *Node) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	n.meter.add("eth_gasPrice")
	return n.eth.SuggestGasPrice(ctx)
}

func (n *Node) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	n.meter.add("eth_maxPriorityFeePerGas")
	return n.eth.SuggestGasTipCap(ctx)
}

func (n *Node) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	n.meter.add("eth_estimateGas")
	return n.eth.EstimateGas(ctx, msg)
}

func (n *Node) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	n.meter.add("eth_sendRawTransaction")
	return n.eth.SendTransaction(ctx, tx)
}

func (n *Node) TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	n.meter.add("eth_getTransactionReceipt")
	return n.eth.TransactionReceipt(ctx, hash)
}

func (n *Node) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	n.meter.add("eth_getBlockByNumber")
	return n.eth.HeaderByNumber(ctx, number)
}

// --------------------------------------------------------------------------
// Dev-chain conveniences
// --------------------------------------------------------------------------

// Accounts lists accounts the node itself holds.
//
// Only a dev node (geth --dev) has any, which is exactly the case this supports:
// bootstrapping a demo chain without managing a keystore.
func (n *Node) Accounts(ctx context.Context) ([]common.Address, error) {
	var out []common.Address
	n.meter.add("eth_accounts")
	if err := n.rpc.CallContext(ctx, &out, "eth_accounts"); err != nil {
		return nil, err
	}
	return out, nil
}

// SendUnsigned asks the node to sign and send on behalf of an unlocked account.
//
// Dev only. A production node has no unlocked accounts and will reject this; the
// publisher signs locally with its own key instead.
func (n *Node) SendUnsigned(ctx context.Context, from common.Address, to *common.Address, value *big.Int, data []byte, gas uint64) (common.Hash, error) {
	arg := map[string]any{
		"from": from.Hex(),
	}
	if to != nil {
		arg["to"] = to.Hex()
	}
	if value != nil && value.Sign() != 0 {
		arg["value"] = "0x" + value.Text(16)
	}
	if len(data) > 0 {
		arg["input"] = "0x" + common.Bytes2Hex(data)
	}
	if gas > 0 {
		arg["gas"] = "0x" + big.NewInt(int64(gas)).Text(16)
	}

	var h common.Hash
	n.meter.add("eth_sendTransaction")
	if err := n.rpc.CallContext(ctx, &h, "eth_sendTransaction", arg); err != nil {
		return common.Hash{}, err
	}
	return h, nil
}
