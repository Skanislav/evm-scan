package lens

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/evmlog"
)

// TestWireMatchesABI is the guard that lets the rest of this package trust its
// structs. Packing matches fields to ABI components by name and unpacking copies them
// by position, so a field renamed, reordered or inserted in AssetLens.sol would
// otherwise be found at runtime — as a wrong balance, not as an error.
func TestWireMatchesABI(t *testing.T) {
	a, err := load()
	if err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	if len(a.in) != 1 || len(a.out) != 1 {
		t.Fatalf("expected one argument each way, got in=%d out=%d", len(a.in), len(a.out))
	}
	checkTuple(t, "Request", a.in[0].Type, reflect.TypeOf(wireRequest{}))
	checkTuple(t, "Result", a.out[0].Type, reflect.TypeOf(wireResult{}))
}

func checkTuple(t *testing.T, path string, typ abi.Type, rt reflect.Type) {
	t.Helper()
	if typ.T != abi.TupleTy {
		t.Fatalf("%s: ABI type is not a tuple", path)
	}
	if rt.Kind() != reflect.Struct {
		t.Fatalf("%s: Go type %s is not a struct", path, rt)
	}
	if got, want := rt.NumField(), len(typ.TupleRawNames); got != want {
		t.Fatalf("%s: Go struct has %d fields, ABI tuple has %d", path, got, want)
	}
	for i, name := range typ.TupleRawNames {
		field := rt.Field(i)
		if want := abi.ToCamelCase(name); field.Name != want {
			t.Errorf("%s: field %d is %q, ABI says %q", path, i, field.Name, want)
		}
		// Descend into nested tuples, including the common tuple[] case.
		elem, ft := typ.TupleElems[i], field.Type
		for elem.T == abi.SliceTy || elem.T == abi.ArrayTy {
			elem = elem.Elem
			if ft.Kind() != reflect.Slice && ft.Kind() != reflect.Array {
				t.Fatalf("%s.%s: ABI is a list, Go type %s is not", path, name, ft)
			}
			ft = ft.Elem()
		}
		if elem.T == abi.TupleTy {
			checkTuple(t, path+"."+name, *elem, ft)
		}
	}
}

// TestEncodeIsCreationCodePlusArguments pins the deployless calling convention: the
// payload has to be the contract's own creation code with the request appended, or
// the node is running something other than the lens we compiled.
func TestEncodeIsCreationCodePlusArguments(t *testing.T) {
	a, err := load()
	if err != nil {
		t.Fatal(err)
	}
	req := wireRequest{
		Account:        common.HexToAddress("0xfeed"),
		Spenders:       []common.Address{common.HexToAddress("0xbeef")},
		Tokens:         []wireTokenQuery{{Token: common.HexToAddress("0xcafe"), Ids: []*big.Int{big.NewInt(7)}}},
		IncludeUri:     true,
		EnumerateLimit: big.NewInt(0),
		GasPerCall:     big.NewInt(0),
		MaxStringBytes: big.NewInt(0),
	}

	payload, err := encode(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(payload), string(a.creation)) {
		t.Fatal("payload does not start with the creation code")
	}
	if len(payload) > MaxPayloadBytes {
		t.Errorf("payload is %d bytes, over EIP-3860's %d", len(payload), MaxPayloadBytes)
	}

	values, err := a.in.Unpack(payload[len(a.creation):])
	if err != nil {
		t.Fatalf("constructor arguments do not decode: %v", err)
	}
	var back struct{ Request wireRequest }
	if err := a.in.Copy(&back, values); err != nil {
		t.Fatal(err)
	}
	if back.Request.Account != req.Account || !back.Request.IncludeUri {
		t.Errorf("round trip lost fields: %+v", back.Request)
	}
	if len(back.Request.Tokens) != 1 || back.Request.Tokens[0].Ids[0].Int64() != 7 {
		t.Errorf("round trip lost the token query: %+v", back.Request.Tokens)
	}
}

// TestDecodeAndView covers the reply path: the positional copy out of the ABI, and
// the rule that an unanswered read stays nil rather than becoming zero.
func TestDecodeAndView(t *testing.T) {
	spender := common.HexToAddress("0x5eed")
	owner := common.HexToAddress("0x0f00")
	reply := wireResult{
		Chain: wireChainInfo{
			ChainId:     big.NewInt(1),
			BlockNumber: big.NewInt(21_000_000),
			ParentHash:  common.HexToHash("0xabc"),
			Timestamp:   big.NewInt(1_700_000_000),
			BaseFee:     big.NewInt(7),
		},
		Account: wireAccountInfo{
			Account:     common.HexToAddress("0xfeed"),
			Balance:     big.NewInt(1234),
			CodeHash:    common.HexToHash("0xdef"),
			CodeSize:    big.NewInt(23),
			IsDelegated: true,
			Delegate:    common.HexToAddress("0xd00d"),
			Code:        []byte{},
		},
		Tokens: []wireTokenInfo{
			{
				Token:          common.HexToAddress("0xcafe"),
				IsContract:     true,
				Standard:       uint8(evmlog.StandardERC20),
				Symbol:         "OK",
				Name:           "Fine Token",
				Decimals:       18,
				HasDecimals:    true,
				TotalSupply:    big.NewInt(0),
				HasTotalSupply: false,
				Balance:        big.NewInt(500),
				HasBalance:     true,
				Allowances:     []*big.Int{big.NewInt(99)},
				AllowanceKnown: []bool{true},
				ApprovedForAll: []bool{false},
				Ids:            []wireTokenIdInfo{},
			},
			{
				Token:      common.HexToAddress("0xbabe"),
				IsContract: true,
				Standard:   uint8(evmlog.StandardERC721),
				IsErc721:   true,
				Symbol:     "NFT",
				// Nothing answered balanceOf: that must not read as a balance of 0.
				TotalSupply:    big.NewInt(0),
				Balance:        big.NewInt(0),
				Allowances:     []*big.Int{big.NewInt(0)},
				AllowanceKnown: []bool{false},
				ApprovedForAll: []bool{true},
				Ids: []wireTokenIdInfo{{
					Id: big.NewInt(42), Owner: owner, OwnerKnown: true,
					Balance: big.NewInt(1), BalanceKnown: true,
					Approved: common.Address{}, Uri: "ipfs://x",
				}},
			},
		},
	}

	encoded := mustPackReply(t, reply)
	got, err := decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := got.view()
	for _, wt := range got.Tokens {
		res.Tokens = append(res.Tokens, viewToken(wt, []common.Address{spender}))
	}

	if res.BlockNumber != 21_000_000 || res.ChainID != 1 {
		t.Errorf("chain info lost: %+v", res)
	}
	if !res.Account.IsDelegated || res.Account.Delegate != common.HexToAddress("0xd00d") {
		t.Errorf("delegation lost: %+v", res.Account)
	}
	if res.Account.NonceKnown {
		t.Error("nonce cannot come from the lens; it must stay unknown until the RPC fills it")
	}
	if len(res.Tokens) != 2 {
		t.Fatalf("got %d tokens", len(res.Tokens))
	}

	erc20 := res.Tokens[0]
	if erc20.Symbol != "OK" {
		t.Errorf("symbol = %q", erc20.Symbol)
	}
	if erc20.Standard != evmlog.StandardERC20 {
		t.Errorf("standard = %v", erc20.Standard)
	}
	if erc20.Decimals == nil || *erc20.Decimals != 18 {
		t.Errorf("decimals = %v", erc20.Decimals)
	}
	if erc20.TotalSupply != nil {
		t.Errorf("total supply was not reported; want nil, got %v", erc20.TotalSupply)
	}
	if erc20.Balance == nil || erc20.Balance.Int64() != 500 {
		t.Errorf("balance = %v", erc20.Balance)
	}
	if len(erc20.Allowances) != 1 || erc20.Allowances[0].Spender != spender ||
		erc20.Allowances[0].Amount.Int64() != 99 {
		t.Errorf("allowance = %+v", erc20.Allowances)
	}

	nft := res.Tokens[1]
	if nft.Balance != nil {
		t.Errorf("unanswered balanceOf must stay nil, got %v", nft.Balance)
	}
	if nft.Allowances[0].Amount != nil || !nft.Allowances[0].ApprovedForAll {
		t.Errorf("operator approval = %+v", nft.Allowances[0])
	}
	if len(nft.IDs) != 1 || nft.IDs[0].Owner == nil || *nft.IDs[0].Owner != owner {
		t.Fatalf("token id lost: %+v", nft.IDs)
	}
	if nft.IDs[0].Approved != nil {
		t.Errorf("getApproved did not answer; want nil, got %v", nft.IDs[0].Approved)
	}
	if nft.IDs[0].URI != "ipfs://x" {
		t.Errorf("uri = %q", nft.IDs[0].URI)
	}
}

// TestQueryHalvesOnOversizedReply is the EIP-170 story: the constructor's return
// value is code, so too large a portfolio fails the call outright. The client has to
// shrink the batch rather than surface that as "your account cannot be read".
func TestQueryHalvesOnOversizedReply(t *testing.T) {
	node := &fakeNode{t: t, maxTokens: 3, block: 100}
	req := Request{Account: common.HexToAddress("0xfeed"), SkipNonce: true}
	for i := range 10 {
		req.Tokens = append(req.Tokens, TokenQuery{Token: common.BigToAddress(big.NewInt(int64(i)))})
	}

	res, err := Query(context.Background(), node, req)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Tokens) != 10 {
		t.Fatalf("got %d tokens back, want 10", len(res.Tokens))
	}
	// Order is the caller's order: a wallet lines these up against its own list.
	for i, tok := range res.Tokens {
		if want := common.BigToAddress(big.NewInt(int64(i))); tok.Address != want {
			t.Errorf("token %d is %s, want %s", i, tok.Address, want)
		}
	}
	if node.calls < 4 {
		t.Errorf("expected the batch to be split, took %d calls", node.calls)
	}
	if !res.Atomic {
		t.Error("every call landed on the same block; result should still be atomic")
	}
	if res.Calls != node.calls {
		t.Errorf("reported %d calls, made %d", res.Calls, node.calls)
	}
}

// TestQuerySplitsIDsWhenOneTokenIsTooLarge: batching by token bottoms out at one, and
// an NFT collection queried for a few hundred ids can still overflow the reply on its
// own. Splitting the ids is the only way out; without it a large query dead-ends.
func TestQuerySplitsIDsWhenOneTokenIsTooLarge(t *testing.T) {
	node := &fakeNode{t: t, maxTokens: 4, maxIDs: 2, block: 100}
	token := common.HexToAddress("0xcafe")

	var ids []*big.Int
	for i := range 9 {
		ids = append(ids, big.NewInt(int64(i)))
	}
	res, err := Query(context.Background(), node, Request{
		Account:   common.HexToAddress("0xfeed"),
		Tokens:    []TokenQuery{{Token: token, IDs: ids}},
		SkipNonce: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Tokens) != 1 {
		t.Fatalf("got %d tokens, want the one that was asked about", len(res.Tokens))
	}
	// The pieces come back as one token with its ids in the requested order.
	if len(res.Tokens[0].IDs) != len(ids) {
		t.Fatalf("got %d ids, want %d", len(res.Tokens[0].IDs), len(ids))
	}
	for i, id := range res.Tokens[0].IDs {
		if id.ID.Int64() != int64(i) {
			t.Errorf("id %d is %s", i, id.ID)
		}
	}
}

// TestQueryDropsAtomicWhenHeadMoves: split across blocks the answer is still useful,
// but it is no longer a snapshot, and saying so is the whole point of the flag.
func TestQueryDropsAtomicWhenHeadMoves(t *testing.T) {
	node := &fakeNode{t: t, maxTokens: 2, block: 100, advance: true}
	req := Request{Account: common.HexToAddress("0xfeed"), SkipNonce: true}
	for i := range 4 {
		req.Tokens = append(req.Tokens, TokenQuery{Token: common.BigToAddress(big.NewInt(int64(i)))})
	}

	res, err := Query(context.Background(), node, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Atomic {
		t.Error("head moved between calls; result is not a single-block snapshot")
	}
}

func TestQueryWithNoTokensStillReadsTheAccount(t *testing.T) {
	node := &fakeNode{t: t, maxTokens: 8, block: 100, nonce: 12}
	res, err := Query(context.Background(), node, Request{Account: common.HexToAddress("0xfeed")})
	if err != nil {
		t.Fatal(err)
	}
	if node.calls != 1 {
		t.Errorf("made %d calls for an account with no tokens", node.calls)
	}
	if !res.Account.NonceKnown || res.Account.Nonce != 12 {
		t.Errorf("nonce = %d known=%v", res.Account.Nonce, res.Account.NonceKnown)
	}
}

func TestQueryPropagatesNodeErrors(t *testing.T) {
	node := &fakeNode{t: t, maxTokens: 8, block: 100, fail: errors.New("no backend")}
	_, err := Query(context.Background(), node, Request{Account: common.HexToAddress("0xfeed")})
	if err == nil || !strings.Contains(err.Error(), "no backend") {
		t.Fatalf("err = %v, want the node's own failure", err)
	}
}

// --------------------------------------------------------------------------
// A node that speaks the deployless convention
// --------------------------------------------------------------------------

// fakeNode answers eth_call by decoding the constructor arguments out of the payload
// and encoding a reply, which exercises both halves of the wire format. It refuses
// batches larger than maxTokens the way a node refuses an oversized reply.
type fakeNode struct {
	t         *testing.T
	maxTokens int
	// maxIDs, when set, refuses a token carrying more ids than this.
	maxIDs  int
	block   uint64
	nonce   uint64
	advance bool
	fail    error
	calls   int
}

func (f *fakeNode) CallAtHead(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	f.t.Helper()
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	if msg.To != nil {
		f.t.Fatalf("deployless call must have no `to` address, got %s", msg.To)
	}

	a, err := load()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(msg.Data), string(a.creation)) {
		f.t.Fatal("call payload is not the lens creation code")
	}
	values, err := a.in.Unpack(msg.Data[len(a.creation):])
	if err != nil {
		return nil, err
	}
	var in struct{ Request wireRequest }
	if err := a.in.Copy(&in, values); err != nil {
		return nil, err
	}
	if len(in.Request.Tokens) > f.maxTokens {
		return nil, errors.New("max code size exceeded")
	}
	for _, q := range in.Request.Tokens {
		if f.maxIDs > 0 && len(q.Ids) > f.maxIDs {
			return nil, errors.New("max code size exceeded")
		}
	}

	if f.advance {
		f.block++
	}
	out := wireResult{
		Chain: wireChainInfo{
			ChainId:     big.NewInt(1),
			BlockNumber: new(big.Int).SetUint64(f.block),
			Timestamp:   big.NewInt(1),
			BaseFee:     big.NewInt(1),
		},
		Account: wireAccountInfo{
			Account: in.Request.Account, Balance: big.NewInt(1),
			CodeSize: big.NewInt(0), Code: []byte{},
		},
	}
	for _, q := range in.Request.Tokens {
		ids := make([]wireTokenIdInfo, len(q.Ids))
		for i, id := range q.Ids {
			ids[i] = wireTokenIdInfo{Id: id, Balance: big.NewInt(1), BalanceKnown: true}
		}
		out.Tokens = append(out.Tokens, wireTokenInfo{
			Token: q.Token, IsContract: true, Standard: uint8(evmlog.StandardERC20),
			Balance: big.NewInt(1), HasBalance: true,
			TotalSupply: big.NewInt(0),
			Allowances:  []*big.Int{}, AllowanceKnown: []bool{}, ApprovedForAll: []bool{},
			Ids: ids,
		})
	}
	return mustPackReply(f.t, out), nil
}

func (f *fakeNode) NonceAt(context.Context, common.Address) (uint64, error) { return f.nonce, nil }

func (f *fakeNode) ChainID(context.Context) (uint64, error)   { return 1, nil }
func (f *fakeNode) HeadBlock(context.Context) (uint64, error) { return f.block, nil }
func (f *fakeNode) HeaderHash(context.Context, uint64) (common.Hash, error) {
	return common.Hash{}, nil
}
func (f *fakeNode) Logs(context.Context, chain.Query) ([]types.Log, error) { return nil, nil }
func (f *fakeNode) SubscribeLogs(context.Context, chain.Query, chan<- types.Log) (ethereum.Subscription, error) {
	return nil, chain.ErrNotStreaming
}
func (f *fakeNode) CodeAt(context.Context, common.Address) ([]byte, error) { return nil, nil }
func (f *fakeNode) Endpoint() chain.Endpoint                               { return chain.Endpoint{} }
func (f *fakeNode) Close()                                                 {}

func mustPackReply(t *testing.T, res wireResult) []byte {
	t.Helper()
	a, err := load()
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.out.Pack(res)
	if err != nil {
		t.Fatalf("pack reply: %v", err)
	}
	return out
}
