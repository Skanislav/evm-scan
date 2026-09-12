package hintreg

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Submitter gets calldata executed on the registry chain and reports the result.
//
// It is the one place the publisher's "how does gas get paid" question lives. The
// shipped implementation is an EOA that signs and pays for itself; a paymaster, a
// relayer or an ERC-4337 account would each be another implementation of this
// interface and nothing else in the publisher would change.
//
// Submit and Wait are separate on purpose: the reference Submit hands back is
// persisted before anyone waits on it, so a crash between the two resumes the wait
// instead of submitting a second time.
// Signer attests to a digest with the publisher's key. Its one consumer is the
// signed ENS gateway: a resolver on a chain the registry is not on cannot verify a
// proof against the root, so it checks that the publisher said so, recently.
//
// Kept apart from Submitter on purpose. Submitting spends gas and is budgeted;
// signing is free and answers a public endpoint, and a type that can only do the
// second cannot be talked into the first.
type Signer interface {
	// Sender is the address a signature recovers to.
	Sender() common.Address
	// Sign returns a 65-byte signature over digest with v in {27, 28}, which is
	// what ecrecover and OpenZeppelin's ECDSA.recover expect.
	Sign(digest [32]byte) ([]byte, error)
}

type Submitter interface {
	// Sender is the address the registry will see as msg.sender.
	Sender() common.Address
	// Submit sends a call and returns a durable reference to it.
	Submit(ctx context.Context, to common.Address, value *big.Int, data []byte) (common.Hash, error)
	// Wait blocks until the referenced submission has a receipt. The receipt comes
	// from the local, verified node, never from whoever carried the submission.
	Wait(ctx context.Context, ref common.Hash) (*types.Receipt, error)
}
