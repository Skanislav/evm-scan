// Command evmscan-verify independently checks a published index commitment.
//
// It is the "don't trust, verify" half of the design. Given an account, it:
//
//  1. fetches the inclusion proof from the API,
//  2. recomputes the asset digest and leaf *locally* from the returned asset list,
//     rather than trusting the digests the API reported,
//  3. replays the merkle proof in Go, and
//  4. calls HintRegistry.verifyInclusion on-chain, so the Solidity verifier and the
//     Go tree builder have to agree.
//
// A consumer that cares can go one step further and re-derive the asset list from the
// chain's own logs; nothing in this pipeline has to be believed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/ens"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/merkle"
)

type proofResponse struct {
	Account    string   `json:"account"`
	Assets     []string `json:"assets"`
	AssetsHash string   `json:"assets_hash"`
	Leaf       string   `json:"leaf"`
	LeafIndex  int      `json:"leaf_index"`
	Proof      []string `json:"proof"`
	Epoch      struct {
		ID         int64  `json:"id"`
		ChainID    uint64 `json:"chain_id"`
		MerkleRoot string `json:"merkle_root"`
		LeafCount  int64  `json:"leaf_count"`
		Status     string `json:"status"`
		OnchainID  *int64 `json:"onchain_epoch_id"`
	} `json:"epoch"`
}

func main() {
	var (
		apiURL   = flag.String("api", "http://127.0.0.1:8080", "evmscand API base URL")
		nodeURL  = flag.String("node", "", "geth IPC path or ws/http URL")
		registry = flag.String("registry", "", "HintRegistry address")
		epoch    = flag.Int64("epoch", 0, "local epoch id to verify")
		account  = flag.String("account", "", "account to prove membership for")
		ccipMode = flag.Bool("ccip", false, "resolve HintRegistry.contractsOf through ERC-3668 instead of checking one epoch")
		gateway  = flag.String("gateway", "", "gateway URL template to use with -ccip (default: the registry's own list)")
		chainID  = flag.Uint64("chain", 0, "chain the index is about, for -ccip (default: the node's own chain)")
		snap     = flag.String("snapshot", "", "verify a published index table (path, URL, or - for stdin) against the epoch it claims")
		ensName  = flag.String("ens", "", "resolve a hint name (<hex>.hints.<name>.eth) through ENS and verify its contracts record on-chain")
		ur       = flag.String("universal-resolver", "0xeEeEEEeE14D718C2B47D9923Deab1335E144EeEe", "Universal Resolver to ask with -ens")
		textKey  = flag.String("key", "evmscan.contracts", "text record to read with -ens")
	)
	flag.Parse()

	// -ens needs a node and a name; the registry is an optional cross-check, so it is
	// dispatched before the flags the epoch modes require.
	if *ensName != "" {
		if *nodeURL == "" {
			flag.Usage()
			os.Exit(2)
		}
		if err := runENS(*nodeURL, *ensName, *ur, *textKey, *registry, *gateway); err != nil {
			log.Fatal(err)
		}
		return
	}

	// -snapshot checks a published table against the chain and proves nothing about
	// one account, so it wants neither -account nor, strictly, a node: without one it
	// still reports what the file builds. Its checks live in runSnapshot, so it has to
	// be dispatched before the flags the other two modes require.
	if *snap != "" {
		// -epoch defaults to 0, which is a real epoch id, so "not given" has to be
		// distinguishable: a snapshot names its own epoch and that is the usual path.
		id := int64(-1)
		if isFlagSet("epoch") {
			id = *epoch
		}
		if err := runSnapshot(context.Background(), *snap, *nodeURL, *registry, id); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *nodeURL == "" || *registry == "" || *account == "" {
		flag.Usage()
		os.Exit(2)
	}

	if *ccipMode {
		if err := runCCIP(*nodeURL, *registry, *account, *gateway, *chainID); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(*apiURL, *nodeURL, *registry, *epoch, *account); err != nil {
		log.Fatal(err)
	}
}

// runENS reads the index through ENS. The Universal Resolver finds HintResolver for
// the name, the resolver reverts OffchainLookup at the registry's gateways, and its
// callback hands the answer to HintRegistry for verification against the latest
// finalized root. With -registry it also runs contractsOf directly and insists the
// two lists agree. As with -ccip, nothing here trusts the API.
func runENS(nodeURL, name, urHex, key, registryAddr, gatewayURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if !common.IsHexAddress(urHex) {
		return fmt.Errorf("%q is not an address", urHex)
	}
	node, err := chain.Dial(ctx, nodeURL, false)
	if err != nil {
		return err
	}
	defer node.Close()
	call := func(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
		return node.CallAtHead(ctx, ethereum.CallMsg{To: &to, Data: data})
	}

	resolver, node0, offset, err := ens.FindResolver(ctx, call, common.HexToAddress(urHex), name)
	if err != nil {
		return fmt.Errorf("findResolver(%s): %w", name, err)
	}
	if resolver == (common.Address{}) {
		return fmt.Errorf("%s has no resolver on its path", name)
	}
	fmt.Printf("name       %s\n", name)
	fmt.Printf("namehash   %s\n", node0.Hex())
	fmt.Printf("resolver   %s (%d label(s) up)\n", resolver.Hex(), offset)

	art, err := contracts.Load("HintResolver")
	if err != nil {
		return err
	}
	resABI, err := art.Parsed()
	if err != nil {
		return err
	}
	var urls []string
	if gatewayURL != "" {
		urls = []string{gatewayURL}
	}
	text, err := ens.ResolveText(ctx, call, resABI, resolver, name, key, http.DefaultClient, urls)
	if err != nil {
		return fmt.Errorf("text(%s, %q): %w", name, key, err)
	}
	if key != "evmscan.contracts" {
		fmt.Printf("%s = %q\n", key, text)
		return nil
	}
	list, err := ens.SplitContracts(text)
	if err != nil {
		return err
	}
	fmt.Printf("evmscan.contracts: %d contract(s), verified on-chain against the latest finalized epoch\n", len(list))
	for _, a := range list {
		fmt.Printf("  %s\n", a.Hex())
	}

	if registryAddr == "" {
		return nil
	}
	// Cross-check: the registry's own contractsOf must agree with what ENS served.
	if !common.IsHexAddress(registryAddr) {
		return fmt.Errorf("%q is not an address", registryAddr)
	}
	account, chainID, hasChain, ok := ens.ParseHintName(name)
	if !ok {
		return fmt.Errorf("%s does not start with a hex account label", name)
	}
	if !hasChain {
		out, err := call(ctx, resolver, resABI.Methods["defaultChainId"].ID)
		if err != nil {
			return err
		}
		vals, err := resABI.Unpack("defaultChainId", out)
		if err != nil {
			return err
		}
		chainID = vals[0].(uint64)
	}
	regABI, err := contracts.HintRegistryABI()
	if err != nil {
		return err
	}
	callData, err := ccip.ContractsOfCallData(regABI, chainID, account)
	if err != nil {
		return err
	}
	out, err := ccip.Resolve(ctx, call, regABI, common.HexToAddress(registryAddr), callData, http.DefaultClient, urls)
	if err != nil {
		return fmt.Errorf("contractsOf cross-check: %w", err)
	}
	direct, err := ccip.DecodeContractsOf(regABI, out)
	if err != nil {
		return err
	}
	if len(direct) != len(list) {
		return fmt.Errorf("MISMATCH: ENS served %d contracts, contractsOf %d", len(list), len(direct))
	}
	for i := range direct {
		if direct[i] != list[i] {
			return fmt.Errorf("MISMATCH at %d: ENS %s, contractsOf %s", i, list[i].Hex(), direct[i].Hex())
		}
	}
	fmt.Printf("contractsOf(%d, %s) agrees: same %d contract(s)\n", chainID, account.Hex(), len(direct))
	return nil
}

// runCCIP asks the contract itself. contractsOf reverts with an ERC-3668
// OffchainLookup, a gateway supplies the leaf and proof, and the contract's
// callback verifies them against the latest finalized root before answering.
// Nothing here trusts the API: a gateway that lies gets a revert, not a listing.
func runCCIP(nodeURL, registryAddr, accountHex, gatewayURL string, chainID uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if !common.IsHexAddress(accountHex) {
		return fmt.Errorf("%q is not an address", accountHex)
	}
	if !common.IsHexAddress(registryAddr) {
		return fmt.Errorf("%q is not an address", registryAddr)
	}
	account := common.HexToAddress(accountHex)
	registry := common.HexToAddress(registryAddr)

	node, err := chain.Dial(ctx, nodeURL, false)
	if err != nil {
		return err
	}
	defer node.Close()
	// The chain a commitment is *about* is not the chain the registry sits on:
	// publishIndex, assetKey and contractsOf all take a chainId precisely so an index
	// of one chain can be committed on another where the gas is cheaper. Defaulting to
	// the node's own chain is right for a single-chain deployment and wrong for a split
	// one, where it asks the only question that has no data, so let it be named.
	if chainID == 0 {
		var err error
		if chainID, err = node.ChainID(ctx); err != nil {
			return err
		}
	}
	regABI, err := contracts.HintRegistryABI()
	if err != nil {
		return err
	}

	callData, err := ccip.ContractsOfCallData(regABI, chainID, account)
	if err != nil {
		return err
	}
	call := func(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
		return node.CallAtHead(ctx, ethereum.CallMsg{To: &to, Data: data})
	}
	var urls []string
	if gatewayURL != "" {
		urls = []string{gatewayURL}
	}

	fmt.Printf("contractsOf(%d, %s) via ERC-3668\n", chainID, account.Hex())
	out, err := ccip.Resolve(ctx, call, regABI, registry, callData, http.DefaultClient, urls)
	if err != nil {
		return err
	}
	assets, err := ccip.DecodeContractsOf(regABI, out)
	if err != nil {
		return fmt.Errorf("decode callback: %w", err)
	}
	fmt.Printf("verified on-chain against the latest finalized epoch: %d contract(s)\n", len(assets))
	for _, a := range assets {
		fmt.Printf("  %s\n", a.Hex())
	}
	return nil
}

func run(apiURL, nodeURL, registryAddr string, epochID int64, accountHex string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if !common.IsHexAddress(accountHex) {
		return fmt.Errorf("%q is not an address", accountHex)
	}
	account := common.HexToAddress(accountHex)

	pr, err := fetchProof(ctx, apiURL, epochID, account)
	if err != nil {
		return err
	}
	fmt.Printf("epoch %d  root %s  leaves %d  status %s\n",
		pr.Epoch.ID, pr.Epoch.MerkleRoot, pr.Epoch.LeafCount, pr.Epoch.Status)

	// Recompute rather than trust: the API's own digests are not inputs here.
	assets := make([]common.Address, 0, len(pr.Assets))
	for _, a := range pr.Assets {
		if !common.IsHexAddress(a) {
			return fmt.Errorf("api returned a non-address asset %q", a)
		}
		assets = append(assets, common.HexToAddress(a))
	}
	assetsHash := merkle.AssetsHash(assets)
	leaf := merkle.LeafHash(account, pr.Epoch.ChainID, assetsHash)

	fmt.Printf("\nrecomputed locally from %d assets:\n", len(assets))
	fmt.Printf("  assets_hash  %s\n", assetsHash.Hex())
	fmt.Printf("  leaf         %s\n", leaf.Hex())

	if got := common.HexToHash(pr.AssetsHash); got != assetsHash {
		return fmt.Errorf("MISMATCH: api reported assets_hash %s, recomputed %s", got.Hex(), assetsHash.Hex())
	}
	if got := common.HexToHash(pr.Leaf); got != leaf {
		return fmt.Errorf("MISMATCH: api reported leaf %s, recomputed %s", got.Hex(), leaf.Hex())
	}
	fmt.Println("  digests match what the API reported")

	proof := make([]common.Hash, len(pr.Proof))
	for i, p := range pr.Proof {
		proof[i] = common.HexToHash(p)
	}
	root := common.HexToHash(pr.Epoch.MerkleRoot)

	if !merkle.Verify(root, leaf, proof) {
		return fmt.Errorf("go merkle verification FAILED for %s", account.Hex())
	}
	fmt.Printf("\ngo verifier      OK (%d-element proof)\n", len(proof))

	// ------------------------------------------------------------- on-chain
	if pr.Epoch.OnchainID == nil {
		fmt.Println("solidity verifier SKIPPED (commitment not published on-chain yet)")
		return nil
	}

	node, err := chain.Dial(ctx, nodeURL, false)
	if err != nil {
		return err
	}
	defer node.Close()

	to := common.HexToAddress(registryAddr)
	regABI, err := contracts.HintRegistryABI()
	if err != nil {
		return err
	}

	proofArr := make([][32]byte, len(proof))
	for i, p := range proof {
		proofArr[i] = p
	}
	data, err := regABI.Pack("verifyInclusion",
		big.NewInt(*pr.Epoch.OnchainID), account, [32]byte(assetsHash), proofArr)
	if err != nil {
		return fmt.Errorf("pack verifyInclusion: %w", err)
	}

	out, err := node.CallAtHead(ctx, ethereum.CallMsg{To: &to, Data: data})
	if err != nil {
		return fmt.Errorf("call verifyInclusion: %w", err)
	}
	vals, err := regABI.Unpack("verifyInclusion", out)
	if err != nil {
		return fmt.Errorf("unpack verifyInclusion: %w", err)
	}
	ok, _ := vals[0].(bool)
	if !ok {
		return fmt.Errorf("solidity verifier REJECTED the proof for %s", account.Hex())
	}

	fmt.Printf("solidity verifier OK (HintRegistry %s, on-chain epoch %d)\n",
		to.Hex(), *pr.Epoch.OnchainID)
	fmt.Printf("\n%s is provably committed to %d assets in an on-chain root.\n",
		account.Hex(), len(assets))

	// Who settles a dispute over this root decides what the root is worth, so report it
	// alongside the proof rather than leaving the consumer to go and look it up.
	client, err := hintreg.NewClient(node, to)
	if err != nil {
		return err
	}
	return reportAdjudication(ctx, client, *pr.Epoch.OnchainID)
}

// reportAdjudication prints how the registry settles disputes and where the commitment
// stands in that process.
func reportAdjudication(ctx context.Context, client *hintreg.Client, onchainID int64) error {
	mode, err := client.Mode(ctx)
	if err != nil {
		return err
	}
	on, err := client.GetEpoch(ctx, onchainID)
	if err != nil {
		return err
	}

	fmt.Printf("\nregistry adjudication: %s\n", mode.String())
	fmt.Printf("on-chain epoch %d: status %s", onchainID, on.Status)
	switch {
	case mode.OracleMode() && on.AssertionID != (common.Hash{}):
		fmt.Printf(", oracle assertion %s\n", on.AssertionID.Hex())
	case on.Status == hintreg.EpochProposed:
		fmt.Printf(", challengeable until unix %d\n", on.ChallengeDeadline)
	default:
		fmt.Println()
	}
	if on.Status != hintreg.EpochFinalized {
		fmt.Println("note: this root is not final yet — it can still be disputed and rejected.")
	}
	return nil
}

func fetchProof(ctx context.Context, apiURL string, epochID int64, account common.Address) (*proofResponse, error) {
	u := fmt.Sprintf("%s/v1/epochs/%d/proof?account=%s", apiURL, epochID, url.QueryEscape(account.Hex()))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch proof: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		return nil, fmt.Errorf("proof request returned %s: %s", resp.Status, body[:n])
	}

	var pr proofResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decode proof: %w", err)
	}
	return &pr, nil
}

// isFlagSet reports whether a flag was given on the command line, as opposed to
// holding its zero default.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
