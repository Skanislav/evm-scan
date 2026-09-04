package hintreg

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/store"
)

// ErrEmptyIndex is returned when there is nothing to commit to.
var ErrEmptyIndex = errors.New("hintreg: index is empty, nothing to commit")

// Publisher builds merkle commitments over the local index and posts them on-chain.
type Publisher struct {
	client   *Client
	sender   chain.Sender
	st       *store.Store
	key      *ecdsa.PrivateKey
	from     common.Address
	regChain *big.Int
	log      *slog.Logger
}

// NewPublisher wires a publisher to a signing key.
func NewPublisher(c *Client, sender chain.Sender, st *store.Store, key *ecdsa.PrivateKey, regChainID uint64, log *slog.Logger) *Publisher {
	from := crypto.PubkeyToAddress(key.PublicKey)
	return &Publisher{
		client:   c,
		sender:   sender,
		st:       st,
		key:      key,
		from:     from,
		regChain: new(big.Int).SetUint64(regChainID),
		log:      log.With("publisher", from.Hex()),
	}
}

// Address is the publisher's on-chain identity.
func (p *Publisher) Address() common.Address { return p.from }

// Build computes a commitment over the index as of the chain's current coverage and
// stores it locally. It does not touch the chain.
//
// A commitment is a cumulative snapshot rather than a delta: one root answers "which
// contracts has this account ever touched", which is the question a wallet actually
// asks. Deltas would force consumers to walk every epoch to get the same answer.
func (p *Publisher) Build(ctx context.Context, chainID uint64, uri string) (store.Epoch, error) {
	from, to, err := p.st.CoverageRange(ctx, chainID)
	if err != nil {
		return store.Epoch{}, err
	}

	sets, err := p.st.SnapshotIndex(ctx, chainID, to)
	if err != nil {
		return store.Epoch{}, err
	}
	if len(sets) == 0 {
		return store.Epoch{}, ErrEmptyIndex
	}

	leaves := make([]common.Hash, len(sets))
	rows := make([]store.EpochLeaf, len(sets))
	for i, s := range sets {
		ah := merkle.AssetsHash(s.Assets)
		leaf := merkle.LeafHash(s.Account, chainID, ah)
		leaves[i] = leaf
		rows[i] = store.EpochLeaf{Index: i, Account: s.Account, AssetsHash: ah, Leaf: leaf}
	}

	tree := merkle.Build(leaves)
	e := store.Epoch{
		ChainID:    chainID,
		FromBlock:  from,
		ToBlock:    to,
		MerkleRoot: tree.Root(),
		LeafCount:  int64(len(leaves)),
		URI:        uri,
		Status:     store.EpochBuilt,
	}

	id, err := p.st.CreateEpoch(ctx, e, rows)
	if err != nil {
		return store.Epoch{}, err
	}
	e.ID = id

	p.log.Info("built index commitment",
		"epoch", id, "chain_id", chainID, "accounts", len(leaves),
		"from_block", from, "to_block", to, "root", e.MerkleRoot.Hex())
	return e, nil
}

// Publish posts a previously built commitment to the registry and waits for its
// receipt.
func (p *Publisher) Publish(ctx context.Context, epochID int64) (common.Hash, error) {
	e, err := p.st.GetEpoch(ctx, epochID)
	if err != nil {
		return common.Hash{}, err
	}
	if e.Status != store.EpochBuilt {
		return common.Hash{}, fmt.Errorf("hintreg: epoch %d is %s, expected %s", epochID, e.Status, store.EpochBuilt)
	}

	bond, err := p.client.PublisherBond(ctx)
	if err != nil {
		return common.Hash{}, err
	}

	data, err := p.client.abi.Pack("publishIndex",
		e.ChainID, e.FromBlock, e.ToBlock, [32]byte(e.MerkleRoot), e.URI)
	if err != nil {
		return common.Hash{}, fmt.Errorf("hintreg: pack publishIndex: %w", err)
	}

	tx, err := p.signedTx(ctx, p.client.Address(), bond, data)
	if err != nil {
		return common.Hash{}, err
	}
	if err := p.sender.SendTransaction(ctx, tx); err != nil {
		return common.Hash{}, fmt.Errorf("hintreg: send publishIndex: %w", err)
	}

	rcpt, err := p.waitReceipt(ctx, tx.Hash())
	if err != nil {
		return tx.Hash(), err
	}
	if rcpt.Status != types.ReceiptStatusSuccessful {
		return tx.Hash(), fmt.Errorf("hintreg: publishIndex reverted (tx %s)", tx.Hash().Hex())
	}

	onchainID, err := p.epochIDFromReceipt(rcpt)
	if err != nil {
		return tx.Hash(), err
	}
	if err := p.st.MarkPublished(ctx, epochID, onchainID, tx.Hash()); err != nil {
		return tx.Hash(), err
	}

	p.log.Info("published index commitment",
		"epoch", epochID, "onchain_epoch", onchainID,
		"root", e.MerkleRoot.Hex(), "tx", tx.Hash().Hex())
	return tx.Hash(), nil
}

// epochIDFromReceipt reads the registry's epoch id out of the IndexPublished log.
func (p *Publisher) epochIDFromReceipt(r *types.Receipt) (int64, error) {
	ev, ok := p.client.abi.Events["IndexPublished"]
	if !ok {
		return 0, errors.New("hintreg: ABI has no IndexPublished event")
	}
	for _, l := range r.Logs {
		if l.Address != p.client.Address() || len(l.Topics) < 2 || l.Topics[0] != ev.ID {
			continue
		}
		// epochId is the first indexed parameter.
		return new(big.Int).SetBytes(l.Topics[1].Bytes()).Int64(), nil
	}
	return 0, errors.New("hintreg: IndexPublished log not found in receipt")
}

// signedTx builds and signs an EIP-1559 transaction.
func (p *Publisher) signedTx(ctx context.Context, to common.Address, value *big.Int, data []byte) (*types.Transaction, error) {
	nonce, err := p.sender.PendingNonceAt(ctx, p.from)
	if err != nil {
		return nil, fmt.Errorf("hintreg: nonce: %w", err)
	}

	tip, err := p.sender.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9)
	}
	head, err := p.sender.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("hintreg: head header: %w", err)
	}
	feeCap := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))

	gas, err := p.sender.EstimateGas(ctx, ethereum.CallMsg{
		From: p.from, To: &to, Value: value, Data: data,
	})
	if err != nil {
		return nil, fmt.Errorf("hintreg: estimate gas: %w", err)
	}
	// Estimation is exact-ish for a view-free call, but a small margin avoids an
	// out-of-gas revert if state shifts between estimate and inclusion.
	gas = gas + gas/5

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   p.regChain,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gas,
		To:        &to,
		Value:     value,
		Data:      data,
	})
	return types.SignTx(tx, types.LatestSignerForChainID(p.regChain), p.key)
}

func (p *Publisher) waitReceipt(ctx context.Context, h common.Hash) (*types.Receipt, error) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	deadline := time.Now().Add(2 * time.Minute)

	for {
		r, err := p.sender.TransactionReceipt(ctx, h)
		if err == nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("hintreg: timed out waiting for receipt %s", h.Hex())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// ProofFor rebuilds a commitment's tree and returns an account's inclusion proof.
func ProofFor(ctx context.Context, st *store.Store, epochID int64, account common.Address) (leaf store.EpochLeaf, proof []common.Hash, err error) {
	leaf, err = st.EpochLeafFor(ctx, epochID, account)
	if err != nil {
		return store.EpochLeaf{}, nil, err
	}
	rows, err := st.EpochLeaves(ctx, epochID)
	if err != nil {
		return store.EpochLeaf{}, nil, err
	}

	leaves := make([]common.Hash, len(rows))
	for i, r := range rows {
		leaves[i] = r.Leaf
	}
	tree := merkle.Build(leaves)
	proof, err = tree.Proof(leaf.Index)
	if err != nil {
		return store.EpochLeaf{}, nil, err
	}
	return leaf, proof, nil
}
