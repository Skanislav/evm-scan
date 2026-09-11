package hintreg

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/store"
)

// TestContractsOfResolvesThroughCCIP runs ERC-3668 end to end against the real
// contract on a simulated chain: contractsOf reverts with OffchainLookup, a gateway
// answers with the leaf and proof, and the callback verifies them against the
// finalized root. It also checks the callback refuses what it must: a stale epoch,
// an unsorted list, a wrong proof.
func TestContractsOfResolvesThroughCCIP(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()
	const window = 5 * time.Second

	// ------------------------------------------------------------- deploy
	art, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := art.Parsed()
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
	h, err := s.sendTx(ctx, nil, nil, append(art.Creation(), ctor...))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	rcpt, err := s.client.TransactionReceipt(ctx, h)
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy receipt: %v", err)
	}
	registry := rcpt.ContractAddress
	client, err := NewClient(s, registry)
	if err != nil {
		t.Fatal(err)
	}

	// Before any epoch exists the lookup says so rather than pointing anywhere.
	callData, _ := ccip.ContractsOfCallData(regABI, chainID, s.from)
	if _, err := s.CallAtHead(ctx, callMsg(registry, callData)); err == nil {
		t.Fatal("contractsOf should revert before a finalized epoch exists")
	} else if rd, ok := ccip.RevertData(err); !ok || ccip.Selector(rd) != errSelector(regABI, "NoFinalizedEpoch") {
		t.Fatalf("want NoFinalizedEpoch, got %v", err)
	}

	// ---------------------------------------------- publish and finalize
	aa := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	bb := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	other := common.HexToAddress("0x0000000000000000000000000000000000000c0d")
	sets := []store.AccountAssetSet{
		{Account: s.from, Assets: []common.Address{bb, aa}}, // unsorted on purpose
		{Account: other, Assets: []common.Address{aa}},
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

	// The tree the gateway must reproduce: same leaves, same order as Build.
	leaves := make([]common.Hash, len(sets))
	for i, set := range sets {
		leaves[i] = merkle.LeafHash(set.Account, chainID, merkle.AssetsHash(set.Assets))
	}
	tree := merkleBuild(leaves)
	if tree.Root() != stored.MerkleRoot {
		t.Fatalf("test tree root %s does not match the built epoch %s", tree.Root().Hex(), stored.MerkleRoot.Hex())
	}
	proofFor := func(i int) []common.Hash {
		p, err := tree.Proof(i)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// ------------------------------------------------------------ gateway
	// Serves the honest answer by default; a test can swap in a bad one.
	var respond func(account common.Address) ([]byte, error)
	respond = func(account common.Address) ([]byte, error) {
		for i, set := range sets {
			if set.Account == account {
				return ccip.EncodeResponse(onchainID, set.Assets, proofFor(i))
			}
		}
		return nil, nil
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.Error(w, "bad path", 400)
			return
		}
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
		account := vals[1].(common.Address)
		resp, err := respond(account)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if resp == nil {
			http.Error(w, "account not in index", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"data": hexutil.Encode(resp)})
	})
	// Served through the client's transport rather than a socket, so the test runs
	// where binding a port is not allowed.
	gw := struct {
		URL    string
		Client func() *http.Client
	}{URL: "http://gateway.test", Client: func() *http.Client { return &http.Client{Transport: inProcess{handler}} }}
	template := gw.URL + "/{sender}/{data}.json"

	data, _ := regABI.Pack("setGateways", []string{template})
	s.mustSend(ctx, registry, nil, data)
	if urls, err := client.Gateways(ctx); err != nil || len(urls) != 1 || urls[0] != template {
		t.Fatalf("gateways: %v %v", urls, err)
	}

	// ------------------------------------------------------------ resolve
	call := func(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
		return s.CallAtHead(ctx, callMsg(to, data))
	}
	out, err := ccip.Resolve(ctx, call, regABI, registry, callData, gw.Client(), nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := ccip.DecodeContractsOf(regABI, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != aa || got[1] != bb {
		t.Fatalf("want [aa bb] sorted, got %v", got)
	}

	// An account the index never saw cannot be answered, and Resolve says so.
	stranger, _ := ccip.ContractsOfCallData(regABI, chainID, common.HexToAddress("0xdead"))
	if _, err := ccip.Resolve(ctx, call, regABI, registry, stranger, gw.Client(), nil); err == nil {
		t.Fatal("unknown account should not resolve")
	}

	// ---------------------------------------------------- refusals
	extraData, err := extraArgs.Pack(chainID, s.from)
	if err != nil {
		t.Fatal(err)
	}
	expectRevert := func(name string, resp []byte) {
		t.Helper()
		cb, _ := regABI.Pack(ccip.MethodCallback, resp, extraData)
		_, err := s.CallAtHead(ctx, callMsg(registry, cb))
		if err == nil {
			t.Fatalf("%s: callback should revert", name)
		}
		rd, ok := ccip.RevertData(err)
		if !ok || ccip.Selector(rd) != errSelector(regABI, name) {
			t.Fatalf("want %s, got %v", name, err)
		}
	}
	stale, _ := ccip.EncodeResponse(onchainID+1, sets[0].Assets, proofFor(0))
	expectRevert("StaleEpoch", stale)
	unsorted, _ := responseArgs.Pack(new(big.Int).SetInt64(onchainID), []common.Address{bb, aa}, hashes(proofFor(0)))
	expectRevert("Unsorted", unsorted)
	wrong, _ := ccip.EncodeResponse(onchainID, []common.Address{aa}, proofFor(0))
	expectRevert("BadProof", wrong)
}

// extraArgs is abi.encode(uint64 chainId, address account); responseArgs is the
// gateway response, spelled out here so a test can encode a deliberately bad one.
var (
	extraArgs    = mustABIArgs("uint64", "address")
	responseArgs = mustABIArgs("uint256", "address[]", "bytes32[]")
)

func mustABIArgs(types ...string) abi.Arguments {
	out := make(abi.Arguments, len(types))
	for i, t := range types {
		ty, err := abi.NewType(t, "", nil)
		if err != nil {
			panic(err)
		}
		out[i] = abi.Argument{Type: ty}
	}
	return out
}

// errSelector is the 4-byte selector of a custom error, as ccip.Selector renders it.
func errSelector(regABI abi.ABI, name string) string {
	id := regABI.Errors[name].ID
	return ccip.Selector(id.Bytes())
}

func callMsg(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}

func hashes(p []common.Hash) [][32]byte {
	out := make([][32]byte, len(p))
	for i, h := range p {
		out[i] = h
	}
	return out
}
