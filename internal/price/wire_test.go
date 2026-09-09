package price

import (
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// TestWireMatchesABI is the same guard internal/lens has: request fields are packed
// by name and reply fields copied by position, so a struct that drifts from
// PriceLens.sol would surface as a wrong price rather than an error.
func TestWireMatchesABI(t *testing.T) {
	a, err := load()
	if err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	if len(a.in) != 1 || len(a.out) != 1 {
		t.Fatalf("expected one argument each way, got in=%d out=%d", len(a.in), len(a.out))
	}
	checkTuple(t, "PriceRequest", a.in[0].Type, reflect.TypeOf(wireRequest{}))
	checkTuple(t, "PriceResult", a.out[0].Type, reflect.TypeOf(wireResult{}))
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

// TestEncodeDecodeRoundTrip drives a full reply through the same ABI the node
// would produce, which is what catches an integer width mapped to the wrong Go
// type: checkTuple only sees names.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	a, err := load()
	if err != nil {
		t.Fatal(err)
	}

	req := wireRequest{
		Tokens:         []common.Address{common.HexToAddress("0xcafe")},
		FeedRegistry:   common.HexToAddress("0x01"),
		Feeds:          []wireFeedHint{{Token: common.HexToAddress("0xcafe"), Aggregator: common.HexToAddress("0x02"), Quote: 1}},
		V3Factory:      common.HexToAddress("0x03"),
		FeeTiers:       []*big.Int{big.NewInt(500)},
		V2Factory:      common.HexToAddress("0x04"),
		QuoteTokens:    []common.Address{common.HexToAddress("0x05")},
		TwapWindow:     1800,
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
	values, err := a.in.Unpack(payload[len(a.creation):])
	if err != nil {
		t.Fatalf("constructor arguments do not decode: %v", err)
	}
	var back struct{ Request wireRequest }
	if err := a.in.Copy(&back, values); err != nil {
		t.Fatal(err)
	}
	if back.Request.TwapWindow != 1800 || len(back.Request.Feeds) != 1 || back.Request.Feeds[0].Quote != 1 {
		t.Errorf("round trip lost fields: %+v", back.Request)
	}

	reply := wireResult{
		Chain: wireChainInfo{ChainId: big.NewInt(1), BlockNumber: big.NewInt(100), Timestamp: big.NewInt(1_700_000_000), BaseFee: big.NewInt(1)},
		Native: wireFeedInfo{
			Aggregator: common.HexToAddress("0xeeee"), Ok: true, Answer: big.NewInt(300_000_000_000), Decimals: 8, HasDecimals: true,
			StartedAt: big.NewInt(1), UpdatedAt: big.NewInt(1_699_999_000), RoundId: big.NewInt(7), AnsweredInRound: big.NewInt(7), Description: "ETH / USD",
		},
		Tokens: []wireTokenPrices{{
			Token: common.HexToAddress("0xcafe"), IsContract: true, Symbol: "CAFE", Decimals: 18, HasDecimals: true,
			Feeds: []wireFeedInfo{{Aggregator: common.HexToAddress("0x02"), Quote: 1, ViaRegistry: true, Ok: true, Answer: big.NewInt(5),
				Decimals: 18, HasDecimals: true, StartedAt: big.NewInt(0), UpdatedAt: big.NewInt(0), RoundId: big.NewInt(1), AnsweredInRound: big.NewInt(1)}},
			Pools: []wirePoolInfo{{
				Pool: common.HexToAddress("0x77"), Kind: 3, Token0: common.HexToAddress("0x05"), Token1: common.HexToAddress("0xcafe"),
				Fee: big.NewInt(3000), SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Tick: big.NewInt(-12), Liquidity: big.NewInt(1e9),
				TwapWindow: 600, TickCumulativeStart: big.NewInt(-1200), TickCumulativeEnd: big.NewInt(-8400),
				Reserve0: big.NewInt(0), Reserve1: big.NewInt(0),
			}},
		}},
	}
	encoded, err := a.out.Pack(reply)
	if err != nil {
		t.Fatalf("pack reply: %v", err)
	}
	if len(encoded) > MaxReplyBytes {
		t.Fatalf("test reply is %d bytes, over the limit", len(encoded))
	}
	got, err := decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	snap := got.view()
	snap.Tokens = []TokenSources{viewToken(got.Tokens[0])}

	if snap.Native == nil || snap.Native.Answer.Int64() != 300_000_000_000 || snap.Native.Description != "ETH / USD" {
		t.Errorf("native = %+v", snap.Native)
	}
	tok := snap.Tokens[0]
	if tok.Symbol != "CAFE" || tok.Decimals == nil || *tok.Decimals != 18 {
		t.Errorf("token = %+v", tok)
	}
	if len(tok.Feeds) != 1 || tok.Feeds[0].Quote != QuoteNative || !tok.Feeds[0].ViaRegistry {
		t.Errorf("feeds = %+v", tok.Feeds)
	}
	if len(tok.Pools) != 1 {
		t.Fatalf("pools = %+v", tok.Pools)
	}
	p := tok.Pools[0]
	if p.Kind != PoolV3 || p.Tick != -12 || p.Fee != 3000 || p.TWAPWindow != 600 ||
		p.TickCumulativeStart.Int64() != -1200 || p.TickCumulativeEnd.Int64() != -8400 {
		t.Errorf("pool = %+v", p)
	}
}
