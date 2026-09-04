package evmlog

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func topic(addr common.Address) common.Hash {
	return common.BytesToHash(addr.Bytes())
}

var (
	alice = common.HexToAddress("0x1111111111111111111111111111111111111111")
	bob   = common.HexToAddress("0x2222222222222222222222222222222222222222")
	carol = common.HexToAddress("0x3333333333333333333333333333333333333333")
)

func TestDecodeDistinguishesERC20FromERC721(t *testing.T) {
	// The two standards share a Transfer signature; only the topic count differs,
	// because ERC-721 indexes the tokenId.
	erc20 := &types.Log{Topics: []common.Hash{SigTransfer, topic(alice), topic(bob)}}
	erc721 := &types.Log{Topics: []common.Hash{SigTransfer, topic(alice), topic(bob), {0x01}}}

	d20, ok := Decode(erc20)
	if !ok {
		t.Fatal("ERC-20 Transfer was not decoded")
	}
	if d20.Standard != StandardERC20 {
		t.Errorf("3-topic Transfer: got %v, want erc20", d20.Standard)
	}

	d721, ok := Decode(erc721)
	if !ok {
		t.Fatal("ERC-721 Transfer was not decoded")
	}
	if d721.Standard != StandardERC721 {
		t.Errorf("4-topic Transfer: got %v, want erc721", d721.Standard)
	}

	for _, d := range []Decoded{d20, d721} {
		if len(d.Participants) != 2 {
			t.Fatalf("want 2 participants, got %d", len(d.Participants))
		}
		if d.Participants[0].Address != alice || d.Participants[0].Role != RoleSender {
			t.Errorf("first participant = %v, want alice as sender", d.Participants[0])
		}
		if d.Participants[1].Address != bob || d.Participants[1].Role != RoleReceiver {
			t.Errorf("second participant = %v, want bob as receiver", d.Participants[1])
		}
	}
}

func TestDecodeDropsZeroAddress(t *testing.T) {
	// A mint's counterparty is the zero address, which is not an account anyone
	// wants surfaced as having "touched" the contract.
	mint := &types.Log{Topics: []common.Hash{SigTransfer, {}, topic(bob)}}

	d, ok := Decode(mint)
	if !ok {
		t.Fatal("mint was not decoded")
	}
	if len(d.Participants) != 1 {
		t.Fatalf("want 1 participant, got %d: %+v", len(d.Participants), d.Participants)
	}
	if d.Participants[0].Address != bob {
		t.Errorf("participant = %s, want bob", d.Participants[0].Address.Hex())
	}
}

func TestDecodeRejectsDirtyTopicHighBytes(t *testing.T) {
	// An unrelated event can collide on topic0 while indexing a non-address value.
	// Admitting it would invent accounts that never existed.
	dirty := common.HexToHash("0xdeadbeef00000000000000004444444444444444444444444444444444444444")
	l := &types.Log{Topics: []common.Hash{SigTransfer, dirty, topic(bob)}}

	d, ok := Decode(l)
	if !ok {
		t.Fatal("expected the log to still decode via its clean topic")
	}
	for _, p := range d.Participants {
		if p.Role == RoleSender {
			t.Errorf("dirty topic was admitted as an address: %s", p.Address.Hex())
		}
	}
}

func TestDecodeERC1155TransferSingle(t *testing.T) {
	l := &types.Log{Topics: []common.Hash{
		SigTransferSingle, topic(carol), topic(alice), topic(bob),
	}}

	d, ok := Decode(l)
	if !ok {
		t.Fatal("TransferSingle was not decoded")
	}
	if d.Standard != StandardERC1155 {
		t.Errorf("standard = %v, want erc1155", d.Standard)
	}
	want := []Participant{
		{Address: carol, Role: RoleOperator},
		{Address: alice, Role: RoleSender},
		{Address: bob, Role: RoleReceiver},
	}
	if len(d.Participants) != len(want) {
		t.Fatalf("got %d participants, want %d", len(d.Participants), len(want))
	}
	for i, w := range want {
		if d.Participants[i] != w {
			t.Errorf("participant %d = %+v, want %+v", i, d.Participants[i], w)
		}
	}
}

func TestDecodeApprovalForAllStaysUncommitted(t *testing.T) {
	// ERC-721 and ERC-1155 both emit ApprovalForAll, so it must not vote for either.
	l := &types.Log{Topics: []common.Hash{SigApprovalForAll, topic(alice), topic(bob)}}

	d, ok := Decode(l)
	if !ok {
		t.Fatal("ApprovalForAll was not decoded")
	}
	if d.Standard != StandardUnknown {
		t.Errorf("standard = %v, want unknown", d.Standard)
	}
	if d.Participants[1].Role != RoleOperator {
		t.Errorf("second participant role = %v, want operator", d.Participants[1].Role)
	}
}

func TestDecodeIgnoresUnrelatedLogs(t *testing.T) {
	for name, l := range map[string]*types.Log{
		"no topics":         {},
		"unknown signature": {Topics: []common.Hash{{0xab}, topic(alice), topic(bob)}},
		"wrong arity":       {Topics: []common.Hash{SigTransfer, topic(alice)}},
		"all zero":          {Topics: []common.Hash{SigTransfer, {}, {}}},
	} {
		if _, ok := Decode(l); ok {
			t.Errorf("%s: expected the log to be ignored", name)
		}
	}
}

func TestInferStandardPrefersStrongerEvidence(t *testing.T) {
	// A two-topic Transfer is the ambiguous default, so anything more specific wins.
	cases := []struct {
		name string
		seen map[Standard]int
		want Standard
	}{
		{"1155 beats 20", map[Standard]int{StandardERC20: 10, StandardERC1155: 1}, StandardERC1155},
		{"721 beats 20", map[Standard]int{StandardERC20: 10, StandardERC721: 1}, StandardERC721},
		{"only 20", map[Standard]int{StandardERC20: 3}, StandardERC20},
		{"nothing", map[Standard]int{}, StandardUnknown},
	}
	for _, tc := range cases {
		if got := InferStandard(tc.seen); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWatchedTopicsCoversEveryDecodableSignature(t *testing.T) {
	// If a signature is decodable but not in the filter, the node never ships it and
	// the decoder silently does nothing.
	watched := map[common.Hash]bool{}
	for _, h := range WatchedTopics()[0] {
		watched[h] = true
	}
	for _, sig := range []common.Hash{
		SigTransfer, SigApproval, SigApprovalForAll, SigTransferSingle, SigTransferBatch,
	} {
		if !watched[sig] {
			t.Errorf("signature %s is decoded but not watched", sig.Hex())
		}
	}
}
