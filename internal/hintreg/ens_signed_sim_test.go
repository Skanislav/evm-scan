package hintreg

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/ens"
)

// TestHintSignedResolverServesSignedRecords drives HintSignedResolver on the
// simulated backend the way ENS on mainnet would: the plain records answer from the
// contract, the offchain ones revert OffchainLookup at an in-process gateway that
// signs with the sim's key, and the callback accepts exactly one signer for exactly
// as long as the answer said.
func TestHintSignedResolverServesSignedRecords(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()
	const parent = "evm-scan.eth"
	registryAddr := common.HexToAddress("0x6D021dBe3A5804F6AC4faE7A20117dF8d7525Ad7")

	// ------------------------------------------------------------- deploy
	art, err := contracts.Load("HintSignedResolver")
	if err != nil {
		t.Fatal(err)
	}
	resABI, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	gwURL := "http://gateway.test"
	ctor, err := resABI.Pack("", s.from, []string{gwURL + "/{sender}/{data}.json"}, chainID, uint64(8453), registryAddr)
	if err != nil {
		t.Fatal(err)
	}
	resolver := deploy(t, s, append(art.Creation(), ctor...))

	call := func(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
		return s.CallAtHead(ctx, callMsg(to, data))
	}
	textProfile := func(name, key string) []byte {
		p, err := ens.TextCallData(ens.Namehash(name), key)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	resolveData := func(name string, profile []byte) []byte {
		data, err := resABI.Pack("resolve", ens.DNSEncode(name), profile)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	unwrap := func(method string, out []byte) string {
		t.Helper()
		vals, err := resABI.Unpack(method, out)
		if err != nil {
			t.Fatalf("unpack %s: %v", method, err)
		}
		str, err := ens.DecodeString(vals[0].([]byte))
		if err != nil {
			t.Fatal(err)
		}
		return str
	}
	expectRevert := func(what string, err error, name string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: should revert", what)
		}
		rd, ok := ccip.RevertData(err)
		if !ok || ccip.Selector(rd) != errSelector(resABI, name) {
			t.Fatalf("%s: want %s, got %v", what, name, err)
		}
	}

	name := ens.HintName(s.from, parent, chainID, false)

	// ------------------------------------------------ records answered locally
	out, err := call(ctx, resolver, resolveData(name, textProfile(name, "evmscan.chain")))
	if err != nil || unwrap("resolve", out) != big.NewInt(int64(chainID)).String() {
		t.Fatalf("evmscan.chain = %v, %v", out, err)
	}
	out, err = call(ctx, resolver, resolveData(name, textProfile(name, "evmscan.registry")))
	if want := "eip155:8453:" + strings.ToLower(registryAddr.Hex()); err != nil || unwrap("resolve", out) != want {
		t.Fatalf("evmscan.registry = %q, %v; want %q", unwrap("resolve", out), err, want)
	}
	out, err = call(ctx, resolver, resolveData(name, textProfile(name, "evmscan.signer")))
	if err != nil || unwrap("resolve", out) != strings.ToLower(s.from.Hex()) {
		t.Fatalf("evmscan.signer = %v, %v", out, err)
	}
	addrCall, _ := ens.AddrCallData(ens.Namehash(name))
	out, err = call(ctx, resolver, resolveData(name, addrCall))
	if err != nil {
		t.Fatal(err)
	}
	if vals, err := resABI.Unpack("resolve", out); err != nil || common.BytesToAddress(vals[0].([]byte)) != s.from {
		t.Fatalf("addr = %v, %v", vals, err)
	}

	// ------------------------------------------------------------ gateway
	// Signs whatever it is asked, with whichever key and expiry the test has set,
	// so the cases below can turn the dials the contract checks.
	signWith := s.key
	expiresAt := func() uint64 { return uint64(s.chainTime(ctx).Add(5 * time.Minute).Unix()) }
	const contractsText = "0x00000000000000000000000000000000000000aa,0x00000000000000000000000000000000000000bb"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.Error(w, "bad path", 400)
			return
		}
		sender := common.HexToAddress(parts[0])
		request, err := hexutil.Decode(strings.TrimSuffix(parts[1], ".json"))
		if err != nil {
			http.Error(w, "bad data", 400)
			return
		}
		vals, err := resABI.Methods["resolve"].Inputs.Unpack(request[4:])
		if err != nil {
			http.Error(w, "bad args", 400)
			return
		}
		wire, _ := vals[0].([]byte)
		profile, _ := vals[1].([]byte)
		if got, err := ens.DNSDecode(wire); err != nil || got != name {
			http.Error(w, "unexpected name", 400)
			return
		}
		key, err := ens.TextKey(profile)
		if err != nil || key != "evmscan.contracts" {
			http.Error(w, "unexpected key", 400)
			return
		}
		result, _ := ens.EncodeString(contractsText)
		expires := expiresAt()
		digest := ccip.SignedResponseHash(sender, expires, request, result)
		sig, _ := crypto.Sign(digest[:], signWith)
		sig[64] += 27
		resp, _ := ccip.EncodeSignedResponse(result, expires, sig)
		_ = json.NewEncoder(w).Encode(map[string]string{"data": hexutil.Encode(resp)})
	})
	hc := &http.Client{Transport: inProcess{handler}}

	// ------------------------------------------------------- the lookup
	req := resolveData(name, textProfile(name, "evmscan.contracts"))
	_, err = call(ctx, resolver, req)
	rd, ok := ccip.RevertData(err)
	if !ok {
		t.Fatalf("evmscan.contracts should revert OffchainLookup, got %v", err)
	}
	lookup, ok, err := ccip.ParseOffchainLookup(rd)
	if err != nil || !ok {
		t.Fatalf("not an OffchainLookup: %v", err)
	}
	if lookup.Sender != resolver || lookup.Callback != [4]byte(resABI.Methods[ccip.MethodResolveWithProof].ID) {
		t.Fatalf("lookup sender/callback = %s %x", lookup.Sender.Hex(), lookup.Callback)
	}
	if string(lookup.CallData) != string(req) || string(lookup.ExtraData) != string(req) {
		t.Fatal("the lookup must carry the resolve calldata as both callData and extraData")
	}

	out, err = ccip.Resolve(ctx, call, resABI, resolver, req, hc, nil)
	if err != nil {
		t.Fatalf("resolve through the gateway: %v", err)
	}
	if got := unwrap(ccip.MethodResolveWithProof, out); got != contractsText {
		t.Fatalf("contracts = %q, want %q", got, contractsText)
	}

	// -------------------------------------------- what the callback refuses
	other, _ := crypto.GenerateKey()
	signWith = other
	_, err = ccip.Resolve(ctx, call, resABI, resolver, req, hc, nil)
	expectRevert("wrong signer", err, "BadSigner")
	signWith = s.key

	expiresAt = func() uint64 { return uint64(s.chainTime(ctx).Add(-time.Second).Unix()) }
	_, err = ccip.Resolve(ctx, call, resABI, resolver, req, hc, nil)
	expectRevert("expired", err, "Expired")
	expiresAt = func() uint64 { return uint64(s.chainTime(ctx).Add(5 * time.Minute).Unix()) }

	// ------------------------------------------------------ rotation
	rotateTo := crypto.PubkeyToAddress(other.PublicKey)
	if err := s.simulateAs(ctx, rotateTo, resolver, mustPack(t, resABI, "setSigner", rotateTo)); err == nil {
		t.Fatal("a non-owner could setSigner")
	}
	s.mustSend(ctx, resolver, nil, mustPack(t, resABI, "setSigner", rotateTo))
	_, err = ccip.Resolve(ctx, call, resABI, resolver, req, hc, nil)
	expectRevert("old key after rotation", err, "BadSigner")
	signWith = other
	if out, err = ccip.Resolve(ctx, call, resABI, resolver, req, hc, nil); err != nil || unwrap(ccip.MethodResolveWithProof, out) != contractsText {
		t.Fatalf("new key after rotation: %v", err)
	}
}

func mustPack(t *testing.T, a abi.ABI, method string, args ...any) []byte {
	t.Helper()
	data, err := a.Pack(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// simulateAs runs a call from an arbitrary sender without sending it, so a test can
// check that a non-owner is refused without funding a second key.
func (s *sim) simulateAs(ctx context.Context, from, to common.Address, data []byte) error {
	msg := callMsg(to, data)
	msg.From = from
	_, err := s.CallAtHead(ctx, msg)
	return err
}

var _ = ecdsa.PrivateKey{}
