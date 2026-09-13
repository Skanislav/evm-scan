package hintreg

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/ens"
)

// TestHintAliasResolverClaimsAndResolves runs the human-readable-name half on a
// simulated chain. A key that never holds a wei signs a claim, the sim's funded
// account carries it — which is the whole point, since the reader pays nothing —
// and `alice.hints.<parent>` then resolves to that account through exactly the code
// path `<hex>.hints.<parent>` uses.
//
// It also pins what a claim must refuse, because every one of those is a way to take
// somebody else's name or spend their signature twice.
func TestHintAliasResolverClaimsAndResolves(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()
	const parent = "hints.evmscan.eth"

	regArt, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := regArt.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	ctor, err := regABI.Pack("", ConstructorArgs(common.Address{}, common.Address{}, s.from, Economics{
		AssetBond: big.NewInt(0), PublisherBond: big.NewInt(0), ChallengeWindow: big.NewInt(5),
		MinFunding: big.NewInt(0), RewardPerBlock: big.NewInt(0),
	}, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	registry := deploy(t, s, append(regArt.Creation(), ctor...))

	resArt, err := contracts.Load("HintAliasResolver")
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

	call := func(to common.Address, data []byte) ([]byte, error) {
		return s.CallAtHead(ctx, callMsg(to, data))
	}
	pack := func(method string, args ...any) []byte {
		t.Helper()
		data, err := resABI.Pack(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	// The claimant holds no ETH, ever. Every transaction below is sent by s.from.
	aliceKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	alice := crypto.PubkeyToAddress(aliceKey.PublicKey)
	if got := s.balance(ctx, alice); got.Sign() != 0 {
		t.Fatalf("claimant should hold nothing, has %v", got)
	}

	nonceOf := func(who common.Address) *big.Int {
		out, err := call(resolver, pack("nonces", who))
		if err != nil {
			t.Fatal(err)
		}
		vals, err := resABI.Unpack("nonces", out)
		if err != nil {
			t.Fatal(err)
		}
		return vals[0].(*big.Int)
	}
	accountOf := func(label string) common.Address {
		out, err := call(resolver, pack("accountOfLabel", crypto.Keccak256Hash([]byte(label))))
		if err != nil {
			t.Fatal(err)
		}
		vals, err := resABI.Unpack("accountOfLabel", out)
		if err != nil {
			t.Fatal(err)
		}
		return vals[0].(common.Address)
	}

	sign := func(key *ecdsa.PrivateKey, primary, label string, account common.Address, nonce, deadline *big.Int) []byte {
		t.Helper()
		td := apitypes.TypedData{
			Types: apitypes.Types{
				"EIP712Domain": {
					{Name: "name", Type: "string"}, {Name: "version", Type: "string"},
					{Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"},
				},
				primary: {
					{Name: "label", Type: "string"}, {Name: "account", Type: "address"},
					{Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint256"},
				},
			},
			PrimaryType: primary,
			Domain: apitypes.TypedDataDomain{
				Name: "evm-scan hint name", Version: "1",
				ChainId: (*math.HexOrDecimal256)(s.chainID), VerifyingContract: resolver.Hex(),
			},
			Message: apitypes.TypedDataMessage{
				"label": label, "account": account.Hex(),
				"nonce": nonce.String(), "deadline": deadline.String(),
			},
		}
		digest, _, err := apitypes.TypedDataAndHash(td)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := crypto.Sign(digest, key)
		if err != nil {
			t.Fatal(err)
		}
		sig[64] += 27 // ECDSA.recover wants v in {27, 28}
		return sig
	}
	refuses := func(what string, data []byte, errName string) {
		t.Helper()
		err := s.simulateAs(ctx, s.from, resolver, data)
		if err == nil {
			t.Fatalf("%s: should revert", what)
		}
		rd, ok := ccip.RevertData(err)
		if !ok || ccip.Selector(rd) != errSelector(resABI, errName) {
			t.Fatalf("%s: want %s, got %v", what, errName, err)
		}
	}

	future := func() *big.Int { return big.NewInt(s.chainTime(ctx).Unix() + 600) }

	// Before the claim the label is not a name: the base class rejects it and there
	// is nothing to fall back to. Asserting this is the difference between the test
	// proving the claim did something and it proving the resolver answers anything.
	if _, err := call(resolver, pack("resolve", ens.DNSEncode("alice."+parent), mustAddrProfile(t, "alice."+parent))); err == nil {
		t.Fatal("alice resolved before it was claimed")
	}

	// ------------------------------------------------------- a claim is carried
	deadline := future()
	sig := sign(aliceKey, "Claim", "alice", alice, nonceOf(alice), deadline)
	s.mustSend(ctx, resolver, nil, pack("claimFor", "alice", alice, deadline, sig))

	if got := accountOf("alice"); got != alice {
		t.Fatalf("accountOfLabel(alice) = %s, want %s", got, alice)
	}
	if got := nonceOf(alice); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("nonce after claim = %v, want 1", got)
	}
	if got := s.balance(ctx, alice); got.Sign() != 0 {
		t.Fatalf("claimant paid something: %v", got)
	}

	// ------------------------------------------ the name resolves like a hex one
	named := "alice." + parent
	hexName := ens.HintName(alice, "evmscan.eth", chainID, false)
	addrOf := func(name string) common.Address {
		t.Helper()
		node := ens.Namehash(name)
		profile, err := ens.AddrCallData(node)
		if err != nil {
			t.Fatal(err)
		}
		out, err := call(resolver, pack("resolve", ens.DNSEncode(name), profile))
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		vals, err := resABI.Unpack("resolve", out)
		if err != nil {
			t.Fatal(err)
		}
		// resolve() returns the profile's own return data; for addr(bytes32) that
		// is one abi-encoded address.
		inner := vals[0].([]byte)
		if len(inner) != 32 {
			t.Fatalf("addr(%s): want 32 bytes, got %d", name, len(inner))
		}
		return common.BytesToAddress(inner[12:32])
	}
	if got := addrOf(named); got != alice {
		t.Fatalf("addr(%s) = %s, want %s", named, got, alice)
	}
	if got := addrOf(hexName); got != alice {
		t.Fatalf("addr(%s) = %s, want %s", hexName, got, alice)
	}

	// The chain-override label works the same under a claimed name.
	if got := addrOf("alice.99." + parent); got != alice {
		t.Fatalf("addr with chain label = %s, want %s", got, alice)
	}

	// ------------------------------------------------------------ what it refuses
	other, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherAddr := crypto.PubkeyToAddress(other.PublicKey)

	// Someone else cannot take a claimed label, even with a perfectly good signature.
	d2 := future()
	sig2 := sign(other, "Claim", "alice", otherAddr, nonceOf(otherAddr), d2)
	refuses("claiming a taken label", pack("claimFor", "alice", otherAddr, d2, sig2), "LabelTaken")

	// A spent nonce cannot be replayed, even by the rightful holder.
	refuses("replaying the first signature", pack("claimFor", "alice", alice, deadline, sig), "BadSigner")

	// An expired deadline is refused even with the right nonce.
	past := big.NewInt(s.chainTime(ctx).Unix() - 1)
	sigPast := sign(aliceKey, "Claim", "bob", alice, nonceOf(alice), past)
	refuses("expired deadline", pack("claimFor", "bob", alice, past, sigPast), "ExpiredSignature")

	// A signature by the wrong key for the right account.
	d3 := future()
	sigWrong := sign(other, "Claim", "bob", alice, nonceOf(alice), d3)
	refuses("wrong signer", pack("claimFor", "bob", alice, d3, sigWrong), "BadSigner")

	// A label that spells an address would shadow that account's own hex name.
	addrLabel := strings.ToLower(otherAddr.Hex()[2:])
	d4 := future()
	sigAddr := sign(aliceKey, "Claim", addrLabel, alice, nonceOf(alice), d4)
	refuses("label that is an address", pack("claimFor", addrLabel, alice, d4, sigAddr), "LabelIsAddress")

	for _, bad := range []string{"ab", "Alice", "al ice", "-alice", "alice-", "alice.eth"} {
		d := future()
		bs := sign(aliceKey, "Claim", bad, alice, nonceOf(alice), d)
		refuses("bad label "+bad, pack("claimFor", bad, alice, d, bs), "BadLabel")
	}

	// ---------------------------------------------------------------- release
	d5 := future()
	sigRel := sign(aliceKey, "Release", "alice", alice, nonceOf(alice), d5)
	s.mustSend(ctx, resolver, nil, pack("releaseFor", "alice", alice, d5, sigRel))
	if got := accountOf("alice"); got != (common.Address{}) {
		t.Fatalf("label still held by %s after release", got)
	}

	// Released, it is anyone's again — and the hex name still answers.
	d6 := future()
	sigOther := sign(other, "Claim", "alice", otherAddr, nonceOf(otherAddr), d6)
	s.mustSend(ctx, resolver, nil, pack("claimFor", "alice", otherAddr, d6, sigOther))
	if got := addrOf(named); got != otherAddr {
		t.Fatalf("addr(%s) after re-claim = %s, want %s", named, got, otherAddr)
	}
	if got := addrOf(hexName); got != alice {
		t.Fatalf("hex name must be unaffected by any claim, got %s", got)
	}

	// An unclaimed label is not a name at all.
	if _, err := call(resolver, pack("resolve", ens.DNSEncode("nobody."+parent), mustAddrProfile(t, "nobody."+parent))); err == nil {
		t.Fatal("an unclaimed label should not resolve")
	}
}

func mustAddrProfile(t *testing.T, name string) []byte {
	t.Helper()
	p, err := ens.AddrCallData(ens.Namehash(name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
