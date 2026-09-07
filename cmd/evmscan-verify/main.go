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
	"github.com/Skanislav/evm-scan/internal/chain"
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
	)
	flag.Parse()

	if *nodeURL == "" || *registry == "" || *account == "" {
		flag.Usage()
		os.Exit(2)
	}

	if err := run(*apiURL, *nodeURL, *registry, *epoch, *account); err != nil {
		log.Fatal(err)
	}
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
