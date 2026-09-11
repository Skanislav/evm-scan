package hintreg

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/ens"
	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/store"
)

// TestHintResolverServesIndexAsENSRecords runs the ENS side end to end on a
// simulated chain: HintResolver parses an account out of a DNS-encoded name, answers
// the plain records from registry state, reverts OffchainLookup for the contracts
// list, and its callback hands the gateway's answer to the registry for
// verification. It also checks what the resolver must refuse.
func TestHintResolverServesIndexAsENSRecords(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()
	const window = 5 * time.Second
	const parent = "evmscan.eth"

	// ------------------------------------------------------------- deploy
	regArt, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := regArt.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	ctor, err := regABI.Pack("", ConstructorArgs(common.Address{}, common.Address{}, s.from, Economics{
		AssetBond: big.NewInt(0), PublisherBond: big.NewInt(0), ChallengeWindow: big.NewInt(int64(window / time.Second)),
		MinFunding: big.NewInt(0), RewardPerBlock: big.NewInt(0),
	}, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	registry := deploy(t, s, append(regArt.Creation(), ctor...))
	client, err := NewClient(s, registry)
	if err != nil {
		t.Fatal(err)
	}

	resArt, err := contracts.Load("HintResolver")
	if err != nil {
		t.Fatal(err)
	}
	resABI, err := resArt.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	rctor, err := resABI.Pack("", registry, chainID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := deploy(t, s, append(resArt.Creation(), rctor...))

	call := func(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
		return s.CallAtHead(ctx, callMsg(to, data))
	}
	// resolveText runs resolve(name, text(node,key)) and decodes the string, or
	// returns the revert.
	resolveRaw := func(name string, profile []byte) ([]byte, error) {
		dns := ens.DNSEncode(name)
		data, err := resABI.Pack("resolve", dns, profile)
		if err != nil {
			t.Fatal(err)
		}
		return call(ctx, resolver, data)
	}
	textProfile := func(name, key string) []byte {
		node := ens.Namehash(name)
		p, err := ens.TextCallData(node, key)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	unwrapString := func(out []byte) string {
		t.Helper()
		vals, err := resABI.Unpack("resolve", out)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ens.DecodeString(vals[0].([]byte))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	expectResolverRevert := func(what string, err error, errABI abi.ABI, name string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: should revert", what)
		}
		rd, ok := ccip.RevertData(err)
		if !ok || ccip.Selector(rd) != errSelector(errABI, name) {
			t.Fatalf("%s: want %s, got %v", what, name, err)
		}
	}

	account := s.from
	name := ens.HintName(account, parent, chainID, false)

	// ------------------------------------------ before any epoch exists
	_, err = resolveRaw(name, textProfile(name, "evmscan.contracts"))
	expectResolverRevert("contracts before an epoch", err, resABI, "NoFinalizedEpoch")
	if out, err := resolveRaw(name, textProfile(name, "evmscan.epoch")); err != nil || unwrapString(out) != "" {
		t.Fatalf("epoch before any epoch: want \"\", got %v %v", out, err)
	}

	// ---------------------------------------------- publish and finalize
	aa := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	bb := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	sets := []store.AccountAssetSet{
		{Account: account, Assets: []common.Address{bb, aa}},
		{Account: common.HexToAddress("0x0c0d"), Assets: []common.Address{aa}},
	}
	st := newMemStore()
	st.sets = sets
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pub := newPublisher(client, s, st, log)
	pub.FinalizeSlack = 0
	pub.now = func() time.Time { return s.chainTime(ctx) }
	e, err := pub.Build(ctx, chainID, "", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := pub.Publish(ctx, e.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	s.pastWindow(window)
	if n, err := pub.FinalizeDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("finalize: n=%d err=%v", n, err)
	}
	stored := mustEpoch(t, st, e.ID)
	onchainID := *stored.OnchainID

	leaves := make([]common.Hash, len(sets))
	for i, set := range sets {
		leaves[i] = merkle.LeafHash(set.Account, chainID, merkle.AssetsHash(set.Assets))
	}
	tree := merkleBuild(leaves)
	proofFor := func(i int) []common.Hash {
		p, err := tree.Proof(i)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// ------------------------------------------------------------ gateway
	// The same gateway shape the daemon runs: it decodes contractsOf calldata and
	// does not care who the sender is. It is served in-process through the HTTP
	// client's transport, so the test needs no listening socket.
	var sawSender string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.Error(w, "bad path", 400)
			return
		}
		sawSender = parts[0]
		data, err := hexutil.Decode(strings.TrimSuffix(parts[1], ".json"))
		if err != nil {
			http.Error(w, "bad data", 400)
			return
		}
		vals, err := regABI.Methods[ccip.MethodContractsOf].Inputs.Unpack(data[4:])
		if err != nil {
			http.Error(w, "bad args", 400)
			return
		}
		who := vals[1].(common.Address)
		for i, set := range sets {
			if set.Account == who {
				resp, _ := ccip.EncodeResponse(onchainID, set.Assets, proofFor(i))
				_ = json.NewEncoder(w).Encode(map[string]string{"data": hexutil.Encode(resp)})
				return
			}
		}
		http.Error(w, "account not in index", 404)
	})
	gw := struct {
		URL    string
		Client func() *http.Client
	}{
		URL: "http://gateway.test",
		Client: func() *http.Client {
			return &http.Client{Transport: inProcess{handler}}
		},
	}
	template := gw.URL + "/{sender}/{data}.json"
	data, _ := regABI.Pack("setGateways", []string{template})
	s.mustSend(ctx, registry, nil, data)

	// ------------------------------------------------------- the lookup
	contractsProfile := textProfile(name, "evmscan.contracts")
	_, err = resolveRaw(name, contractsProfile)
	if err == nil {
		t.Fatal("evmscan.contracts should revert OffchainLookup")
	}
	rd, ok := ccip.RevertData(err)
	if !ok {
		t.Fatalf("no revert data: %v", err)
	}
	lookup, ok, perr := ccip.ParseLookup(resABI, rd)
	if perr != nil || !ok {
		t.Fatalf("not an OffchainLookup: %v %v", perr, err)
	}
	if lookup.Sender != resolver {
		t.Fatalf("lookup sender %s, want the resolver %s", lookup.Sender.Hex(), resolver.Hex())
	}
	if len(lookup.URLs) != 1 || lookup.URLs[0] != template {
		t.Fatalf("lookup urls %v, want the registry's %q", lookup.URLs, template)
	}
	wantCall, _ := ccip.ContractsOfCallData(regABI, chainID, account)
	if hexutil.Encode(lookup.CallData) != hexutil.Encode(wantCall) {
		t.Fatalf("lookup callData is not contractsOf(chainId, account)")
	}
	if lookup.Callback != [4]byte(resABI.Methods["resolveCallback"].ID) {
		t.Fatalf("lookup callback %x, want resolveCallback", lookup.Callback)
	}

	dns := ens.DNSEncode(name)
	resolveData, _ := resABI.Pack("resolve", dns, contractsProfile)
	out, err := ccip.Resolve(ctx, call, resABI, resolver, resolveData, gw.Client(), nil)
	if err != nil {
		t.Fatalf("resolve through CCIP: %v", err)
	}
	// The callback returns bytes; inside is the ABI-encoded string text() promises.
	vals, err := resABI.Unpack("resolveCallback", out)
	if err != nil {
		t.Fatal(err)
	}
	text, err := ens.DecodeString(vals[0].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ToLower(aa.Hex()) + "," + strings.ToLower(bb.Hex())
	if text != want {
		t.Fatalf("evmscan.contracts = %q, want %q", text, want)
	}
	if sawSender != strings.ToLower(resolver.Hex()) {
		t.Fatalf("gateway saw sender %s, want the resolver", sawSender)
	}

	// ------------------------------------------------ the plain records
	if out, err := resolveRaw(name, textProfile(name, "evmscan.epoch")); err != nil || unwrapString(out) != strconv.FormatInt(onchainID, 10) {
		t.Fatalf("evmscan.epoch: %v %v", out, err)
	}
	if out, err := resolveRaw(name, textProfile(name, "evmscan.registry")); err != nil || unwrapString(out) != strings.ToLower(registry.Hex()) {
		t.Fatalf("evmscan.registry: %v %v", out, err)
	}
	if out, err := resolveRaw(name, textProfile(name, "evmscan.chain")); err != nil || unwrapString(out) != strconv.FormatUint(chainID, 10) {
		t.Fatalf("evmscan.chain: %v %v", out, err)
	}
	if out, err := resolveRaw(name, textProfile(name, "evmscan.root")); err != nil || unwrapString(out) != strings.ToLower(stored.MerkleRoot.Hex()) {
		t.Fatalf("evmscan.root: %v %v", out, err)
	}
	if out, err := resolveRaw(name, textProfile(name, "evmscan.range")); err != nil ||
		unwrapString(out) != strconv.FormatUint(stored.FromBlock, 10)+"-"+strconv.FormatUint(stored.ToBlock, 10) {
		t.Fatalf("evmscan.range: %v %v", out, err)
	}
	if out, err := resolveRaw(name, textProfile(name, "avatar")); err != nil || unwrapString(out) != "" {
		t.Fatalf("unknown text key should be empty: %v %v", out, err)
	}

	node := ens.Namehash(name)
	addrProfile, _ := ens.AddrCallData(node)
	out, err = resolveRaw(name, addrProfile)
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	if vals, err := resABI.Unpack("resolve", out); err != nil {
		t.Fatal(err)
	} else if got := common.BytesToAddress(vals[0].([]byte)); got != account {
		t.Fatalf("addr = %s, want %s", got.Hex(), account.Hex())
	}

	// An explicit chain label overrides the default; a chain with no epoch says so.
	otherName := ens.HintName(account, parent, 999, true)
	_, err = resolveRaw(otherName, textProfile(otherName, "evmscan.contracts"))
	expectResolverRevert("contracts on chain 999", err, resABI, "NoFinalizedEpoch")
	if out, err := resolveRaw(otherName, textProfile(otherName, "evmscan.chain")); err != nil || unwrapString(out) != "999" {
		t.Fatalf("evmscan.chain with explicit label: %v %v", out, err)
	}

	// ---------------------------------------------------------- refusals
	contenthash := append(crypto4("contenthash(bytes32)"), make([]byte, 32)...)
	_, err = resolveRaw(name, contenthash)
	expectResolverRevert("contenthash", err, resABI, "UnsupportedResolverProfile")

	bad := "vitalik.hints." + parent
	_, err = resolveRaw(bad, textProfile(bad, "evmscan.chain"))
	expectResolverRevert("non-hex label", err, resABI, "InvalidAccountLabel")

	extraData, _ := extraArgs.Pack(chainID, account)
	stale, _ := ccip.EncodeResponse(onchainID+1, sets[0].Assets, proofFor(0))
	cb, _ := resABI.Pack("resolveCallback", stale, extraData)
	_, err = call(ctx, resolver, cb)
	expectResolverRevert("stale epoch through the resolver", err, regABI, "StaleEpoch")
	wrong, _ := ccip.EncodeResponse(onchainID, []common.Address{aa}, proofFor(0))
	cb, _ = resABI.Pack("resolveCallback", wrong, extraData)
	_, err = call(ctx, resolver, cb)
	expectResolverRevert("bad proof through the resolver", err, regABI, "BadProof")

	// ------------------------------------------------------- interfaces
	for _, tc := range []struct {
		id   [4]byte
		want bool
	}{
		{[4]byte{0x01, 0xff, 0xc9, 0xa7}, true}, // ERC-165
		{[4]byte{0x90, 0x61, 0xb9, 0x23}, true}, // IExtendedResolver
		{[4]byte{0x3b, 0x3b, 0x57, 0xde}, false},
	} {
		data, _ := resABI.Pack("supportsInterface", tc.id)
		out, err := call(ctx, resolver, data)
		if err != nil {
			t.Fatal(err)
		}
		vals, _ := resABI.Unpack("supportsInterface", out)
		if got := vals[0].(bool); got != tc.want {
			t.Fatalf("supportsInterface(%x) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestHintResolverParseName tables the label shapes the resolver accepts.
func TestHintResolverParseName(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	const defaultChain = uint64(11155111)

	resArt, err := contracts.Load("HintResolver")
	if err != nil {
		t.Fatal(err)
	}
	resABI, err := resArt.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	rctor, _ := resABI.Pack("", common.HexToAddress("0x1234"), defaultChain)
	resolver := deploy(t, s, append(resArt.Creation(), rctor...))

	acct := common.HexToAddress("0xAbCdEf0123456789abcdef0123456789ABCDEF01")
	hexLower := strings.ToLower(acct.Hex()[2:])
	cases := []struct {
		name  string
		acct  common.Address
		chain uint64
		ok    bool
	}{
		{hexLower + ".hints.evmscan.eth", acct, defaultChain, true},
		{strings.ToUpper(hexLower) + ".hints.evmscan.eth", acct, defaultChain, true},
		{"0x" + hexLower + ".hints.evmscan.eth", acct, defaultChain, true},
		{hexLower + ".1.hints.evmscan.eth", acct, 1, true},
		{hexLower + ".8453.hints.evmscan.eth", acct, 8453, true},
		{hexLower + ".mainnet.hints.evmscan.eth", acct, defaultChain, true}, // non-decimal second label: default chain
		{hexLower[:39] + ".hints.evmscan.eth", common.Address{}, 0, false},
		{hexLower + "0.hints.evmscan.eth", common.Address{}, 0, false},
		{"zz" + hexLower[2:] + ".hints.evmscan.eth", common.Address{}, 0, false},
		{"vitalik.eth", common.Address{}, 0, false},
	}
	for _, tc := range cases {
		dns := ens.DNSEncode(tc.name)
		data, _ := resABI.Pack("parseName", dns)
		out, err := s.CallAtHead(ctx, callMsg(resolver, data))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		vals, err := resABI.Unpack("parseName", out)
		if err != nil {
			t.Fatal(err)
		}
		gotAcct, gotChain, gotOK := vals[0].(common.Address), vals[1].(uint64), vals[2].(bool)
		if gotOK != tc.ok || (tc.ok && (gotAcct != tc.acct || gotChain != tc.chain)) {
			t.Fatalf("%s: got (%s, %d, %v), want (%s, %d, %v)", tc.name, gotAcct.Hex(), gotChain, gotOK, tc.acct.Hex(), tc.chain, tc.ok)
		}
	}

	// contractsText is the exact wire format clients split on.
	data, _ := resABI.Pack("contractsText", []common.Address{common.HexToAddress("0xaa"), acct})
	out, err := s.CallAtHead(ctx, callMsg(resolver, data))
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := resABI.Unpack("contractsText", out)
	if got, want := vals[0].(string), "0x00000000000000000000000000000000000000aa,"+strings.ToLower(acct.Hex()); got != want {
		t.Fatalf("contractsText = %q, want %q", got, want)
	}
}

// deploy sends creation code and returns the new contract's address.
func deploy(t *testing.T, s *sim, code []byte) common.Address {
	t.Helper()
	ctx := context.Background()
	h, err := s.sendTx(ctx, nil, nil, code)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	rcpt, err := s.client.TransactionReceipt(ctx, h)
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy receipt: %v", err)
	}
	return rcpt.ContractAddress
}

func crypto4(sig string) []byte {
	return ens.Selector(sig)
}

// inProcess serves an http.Handler as an http.RoundTripper, so a client can talk to
// it without a socket.
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}
