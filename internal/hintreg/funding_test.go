package hintreg

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
)

// replyCaller answers every call with one fixed blob, which is all Funding needs.
type replyCaller struct{ out []byte }

func (r replyCaller) CallAtHead(context.Context, ethereum.CallMsg) ([]byte, error) {
	return r.out, nil
}
func (r replyCaller) HeadBlock(context.Context) (uint64, error) { return 0, nil }

func word(v uint64) []byte { return common.LeftPadBytes(new(big.Int).SetUint64(v).Bytes(), 32) }

// A registry deployed before `vouched` existed returns three words. One is live on
// Base, and the rules of this contract are immutable by design — changing them means
// a new deployment — so both shapes will always be out there.
//
// This is not a hypothetical. The field was added, the ABI went to four words, and
// go-ethereum answers a three-word reply with "length insufficient 96 require 128" —
// which internal/api discards silently, so the funding column would simply have
// emptied against the registry that is actually deployed.
func TestFundingAcceptsBothRegistryShapes(t *testing.T) {
	const bal, from, to, vouched = 1_000_000, 5, 20, 7_500_000

	t.Run("current", func(t *testing.T) {
		out := append(append(append(word(bal), word(from)...), word(to)...), word(vouched)...)
		c := &Client{src: replyCaller{out}, addr: common.Address{}}
		var err error
		if c.abi, err = contracts.HintRegistryABI(); err != nil {
			t.Fatal(err)
		}
		f, err := c.Funding(context.Background(), common.Hash{})
		if err != nil {
			t.Fatal(err)
		}
		if f.Balance.Uint64() != bal || f.PaidFrom != from || f.PaidTo != to {
			t.Errorf("got %+v", f)
		}
		if f.Vouched == nil || f.Vouched.Uint64() != vouched {
			t.Errorf("vouched = %v, want %d", f.Vouched, vouched)
		}
	})

	t.Run("pre-vouched", func(t *testing.T) {
		out := append(append(word(bal), word(from)...), word(to)...)
		c := &Client{src: replyCaller{out}, addr: common.Address{}}
		var err error
		if c.abi, err = contracts.HintRegistryABI(); err != nil {
			t.Fatal(err)
		}
		f, err := c.Funding(context.Background(), common.Hash{})
		if err != nil {
			t.Fatalf("the deployed registry's reply was refused: %v", err)
		}
		if f.Balance.Uint64() != bal || f.PaidFrom != from || f.PaidTo != to {
			t.Errorf("got %+v", f)
		}
		// Absent, not zero-substituted from Balance: that registry does not track it,
		// and Balance drains as coverage is claimed where vouched only ever rises.
		if f.Vouched != nil {
			t.Errorf("vouched = %v, want nil for a registry that has no such field", f.Vouched)
		}
	})

	t.Run("neither shape", func(t *testing.T) {
		c := &Client{src: replyCaller{word(1)}, addr: common.Address{}}
		var err error
		if c.abi, err = contracts.HintRegistryABI(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Funding(context.Background(), common.Hash{}); err == nil {
			t.Error("a 32-byte reply was accepted as funding")
		}
	})
}
