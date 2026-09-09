package hintreg

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/chain"
)

// EOASubmitter signs with a private key and pays its own gas.
//
// The key needs a balance. On a registry with `publisherReward` set, every finalized
// epoch tops it back up from the reward pool, so after an initial top-up the account
// sustains itself for as long as people keep paying for indexing.
type EOASubmitter struct {
	sender   chain.Sender
	key      *ecdsa.PrivateKey
	from     common.Address
	chainID  *big.Int
	waitFor  time.Duration
	pollWait time.Duration

	// FallbackGas is used when the node cannot estimate gas. Zero means fail
	// instead, which is the right default against a real node; a light client that
	// lacks eth_estimateGas is the case it exists for.
	FallbackGas uint64
}

// NewEOASubmitter wires a signing key to a node's write surface.
func NewEOASubmitter(sender chain.Sender, key *ecdsa.PrivateKey, chainID uint64, waitFor time.Duration) *EOASubmitter {
	if waitFor <= 0 {
		waitFor = 2 * time.Minute
	}
	return &EOASubmitter{
		sender:   sender,
		key:      key,
		from:     crypto.PubkeyToAddress(key.PublicKey),
		chainID:  new(big.Int).SetUint64(chainID),
		waitFor:  waitFor,
		pollWait: 500 * time.Millisecond,
	}
}

func (s *EOASubmitter) Sender() common.Address { return s.from }

// Submit signs and sends a call, returning its transaction hash.
func (s *EOASubmitter) Submit(ctx context.Context, to common.Address, value *big.Int, data []byte) (common.Hash, error) {
	return s.send(ctx, &to, value, data)
}

// Deploy sends creation bytecode. Not part of Submitter: a smart account cannot
// CREATE, and the only caller is the deployment tool.
func (s *EOASubmitter) Deploy(ctx context.Context, code []byte) (common.Hash, error) {
	return s.send(ctx, nil, nil, code)
}

func (s *EOASubmitter) send(ctx context.Context, to *common.Address, value *big.Int, data []byte) (common.Hash, error) {
	tx, err := s.signedTx(ctx, to, value, data)
	if err != nil {
		return common.Hash{}, err
	}
	if err := s.sender.SendTransaction(ctx, tx); err != nil {
		return common.Hash{}, fmt.Errorf("hintreg: send: %w", err)
	}
	return tx.Hash(), nil
}

// Wait polls for the receipt of a transaction this submitter sent.
func (s *EOASubmitter) Wait(ctx context.Context, ref common.Hash) (*types.Receipt, error) {
	t := time.NewTicker(s.pollWait)
	defer t.Stop()
	deadline := time.Now().Add(s.waitFor)

	for {
		r, err := s.sender.TransactionReceipt(ctx, ref)
		if err == nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("hintreg: timed out waiting for receipt %s", ref.Hex())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// signedTx builds and signs an EIP-1559 transaction.
func (s *EOASubmitter) signedTx(ctx context.Context, to *common.Address, value *big.Int, data []byte) (*types.Transaction, error) {
	nonce, err := s.sender.PendingNonceAt(ctx, s.from)
	if err != nil {
		return nil, fmt.Errorf("hintreg: nonce: %w", err)
	}

	tip, err := s.sender.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9)
	}
	head, err := s.sender.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("hintreg: head header: %w", err)
	}
	if head.BaseFee == nil {
		return nil, fmt.Errorf("hintreg: chain %s has no base fee; only EIP-1559 chains are supported", s.chainID)
	}
	feeCap := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))

	gas, err := s.sender.EstimateGas(ctx, ethereum.CallMsg{
		From: s.from, To: to, Value: value, Data: data,
	})
	if err != nil {
		if s.FallbackGas == 0 {
			return nil, fmt.Errorf("hintreg: estimate gas: %w", err)
		}
		gas = s.FallbackGas
	} else {
		// Estimation is exact-ish for a view-free call, but a small margin avoids an
		// out-of-gas revert if state shifts between estimate and inclusion.
		gas = gas + gas/5
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   s.chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gas,
		To:        to,
		Value:     value,
		Data:      data,
	})
	return types.SignTx(tx, types.LatestSignerForChainID(s.chainID), s.key)
}
