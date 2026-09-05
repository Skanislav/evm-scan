// Command evmscan-demo bootstraps a end-to-end demo on a local geth dev chain.
//
// It deploys the HintRegistry and a few token contracts, generates multi-account
// traffic against them, and registers them as hints. Everything it touches is a
// contract we just deployed on a throwaway chain, and every key it uses is derived
// from a fixed seed, so a run is reproducible and nothing here is a secret.
//
// Usage:
//
//	geth --dev --dev.period 2 --datadir /tmp/devchain --ipcpath /tmp/devchain/geth.ipc
//	evmscan-demo -node /tmp/devchain/geth.ipc -out config.demo.yaml
package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
)

const (
	deployGas = 3_000_000
	callGas   = 300_000
	// Fixed seed: the demo's user keys must be reproducible across runs, and they
	// only ever hold play money on a throwaway dev chain.
	keySeed = "evm-scan demo user"
)

func main() {
	var (
		nodeURL   = flag.String("node", "", "geth IPC path or ws/http URL of the dev node")
		users     = flag.Int("users", 6, "number of demo user accounts")
		out       = flag.String("out", "", "write a ready-to-run evmscand config to this path")
		dsn       = flag.String("dsn", "postgres://evmscan:evmscan@127.0.0.1:5432/evmscan?sslmode=disable", "database DSN to write into the generated config")
		listen    = flag.String("listen", "127.0.0.1:8080", "API listen address for the generated config")
		bondWei   = flag.Int64("bond", 0, "asset and publisher bond in wei")
		challenge = flag.Int64("challenge-window", 60, "challenge window in seconds")
	)
	flag.Parse()

	if *nodeURL == "" {
		log.Fatal("-node is required (e.g. /tmp/devchain/geth.ipc)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := run(ctx, *nodeURL, *users, *out, *dsn, *listen, *bondWei, *challenge); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, nodeURL string, userCount int, outPath, dsn, listen string, bond, challengeWindow int64) error {
	node, err := chain.Dial(ctx, nodeURL, true)
	if err != nil {
		return err
	}
	defer node.Close()

	chainID, err := node.ChainID(ctx)
	if err != nil {
		return err
	}

	accounts, err := node.Accounts(ctx)
	if err != nil || len(accounts) == 0 {
		return fmt.Errorf("no unlocked accounts on the node; this tool expects `geth --dev`: %w", err)
	}
	faucet := accounts[0]

	d := &deployer{node: node, chainID: new(big.Int).SetUint64(chainID), faucet: faucet}
	fmt.Printf("chain id      %d\n", chainID)
	fmt.Printf("faucet        %s\n", faucet.Hex())

	// ---------------------------------------------------------------- deploy
	registry, err := d.deploy(ctx, "HintRegistry", faucet,
		big.NewInt(bond), big.NewInt(bond), big.NewInt(challengeWindow))
	if err != nil {
		return fmt.Errorf("deploy HintRegistry: %w", err)
	}
	fmt.Printf("HintRegistry  %s\n", registry.Hex())

	usdc, err := d.deploy(ctx, "DemoERC20", "Demo USD Coin", "dUSDC")
	if err != nil {
		return fmt.Errorf("deploy DemoERC20: %w", err)
	}
	weth, err := d.deploy(ctx, "DemoERC20", "Demo Wrapped Ether", "dWETH")
	if err != nil {
		return fmt.Errorf("deploy DemoERC20: %w", err)
	}
	nft, err := d.deploy(ctx, "DemoERC721", "Demo Punks", "dPUNK")
	if err != nil {
		return fmt.Errorf("deploy DemoERC721: %w", err)
	}
	// Deliberately never registered in HintRegistry. Runtime discovery has to find
	// this one on its own, which is the whole point of watching the head.
	stealth, err := d.deploy(ctx, "DemoERC20", "Demo Stealth Token", "dSTEALTH")
	if err != nil {
		return fmt.Errorf("deploy DemoERC20: %w", err)
	}
	fmt.Printf("DemoERC20     %s  (dUSDC)\n", usdc.Hex())
	fmt.Printf("DemoERC20     %s  (dWETH)\n", weth.Hex())
	fmt.Printf("DemoERC721    %s  (dPUNK)\n", nft.Hex())
	fmt.Printf("DemoERC20     %s  (dSTEALTH, NOT registered - for discovery)\n", stealth.Hex())

	// ------------------------------------------------------------ publisher
	publisherKey, err := deriveKey("evm-scan demo publisher")
	if err != nil {
		return err
	}
	publisher := crypto.PubkeyToAddress(publisherKey.PublicKey)
	if _, err := d.sendFaucet(ctx, &publisher, ether(10), nil, 21000); err != nil {
		return fmt.Errorf("fund publisher: %w", err)
	}
	fmt.Printf("publisher     %s\n", publisher.Hex())

	// ----------------------------------------------------------- demo users
	wallets := make([]*wallet, userCount)
	for i := range wallets {
		key, err := deriveKey(fmt.Sprintf("%s %d", keySeed, i))
		if err != nil {
			return err
		}
		wallets[i] = &wallet{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}
		if _, err := d.sendFaucet(ctx, &wallets[i].addr, ether(5), nil, 21000); err != nil {
			return fmt.Errorf("fund user %d: %w", i, err)
		}
	}
	if err := d.wait(ctx); err != nil {
		return err
	}
	for i, w := range wallets {
		fmt.Printf("user %-2d       %s\n", i, w.addr.Hex())
	}

	// ------------------------------------------------------- generate traffic
	erc20ABI, err := abiOf("DemoERC20")
	if err != nil {
		return err
	}
	erc721ABI, err := abiOf("DemoERC721")
	if err != nil {
		return err
	}

	fmt.Println("\ngenerating traffic…")

	// Mints from the faucet: every user receives both tokens and an NFT, which is
	// what makes them discoverable at all.
	for _, w := range wallets {
		for _, tok := range []common.Address{usdc, weth, stealth} {
			data, err := erc20ABI.Pack("mint", w.addr, ether(1000))
			if err != nil {
				return err
			}
			if _, err := d.sendFaucet(ctx, &tok, nil, data, callGas); err != nil {
				return err
			}
		}
		data, err := erc721ABI.Pack("mint", w.addr)
		if err != nil {
			return err
		}
		if _, err := d.sendFaucet(ctx, &nft, nil, data, callGas); err != nil {
			return err
		}
	}
	if err := d.wait(ctx); err != nil {
		return err
	}

	// User-to-user activity, so the index contains more than mint counterparties.
	for i, w := range wallets {
		if err := w.load(ctx, node); err != nil {
			return err
		}
		peer := wallets[(i+1)%len(wallets)].addr

		data, err := erc20ABI.Pack("transfer", peer, ether(10))
		if err != nil {
			return err
		}
		if _, err := d.sendFrom(ctx, w, &usdc, nil, data, callGas); err != nil {
			return err
		}

		data, err = erc20ABI.Pack("approve", peer, ether(50))
		if err != nil {
			return err
		}
		if _, err := d.sendFrom(ctx, w, &weth, nil, data, callGas); err != nil {
			return err
		}

		data, err = erc721ABI.Pack("transferFrom", w.addr, peer, big.NewInt(int64(i)))
		if err != nil {
			return err
		}
		if _, err := d.sendFrom(ctx, w, &nft, nil, data, callGas); err != nil {
			return err
		}

		data, err = erc20ABI.Pack("transfer", peer, ether(1))
		if err != nil {
			return err
		}
		if _, err := d.sendFrom(ctx, w, &stealth, nil, data, callGas); err != nil {
			return err
		}
	}
	if err := d.wait(ctx); err != nil {
		return err
	}

	// Spread more stealth-token activity over several blocks so it accumulates the
	// blocks_seen a promotion threshold looks for.
	for round := 0; round < 3; round++ {
		for i, w := range wallets {
			peer := wallets[(i+round+2)%len(wallets)].addr
			data, err := erc20ABI.Pack("transfer", peer, ether(1))
			if err != nil {
				return err
			}
			if _, err := d.sendFrom(ctx, w, &stealth, nil, data, callGas); err != nil {
				return err
			}
		}
		if err := d.wait(ctx); err != nil {
			return err
		}
	}

	// ------------------------------------------------------- register hints
	regABI, err := abiOf("HintRegistry")
	if err != nil {
		return err
	}
	type hint struct {
		addr common.Address
		kind uint8
	}
	for _, h := range []hint{{usdc, 20}, {weth, 20}, {nft, 21}} {
		data, err := regABI.Pack("registerAsset", chainID, h.addr, h.kind, uint64(0))
		if err != nil {
			return err
		}
		if _, err := d.sendFaucet(ctx, &registry, big.NewInt(bond), data, callGas); err != nil {
			return fmt.Errorf("registerAsset %s: %w", h.addr.Hex(), err)
		}
	}
	if err := d.wait(ctx); err != nil {
		return err
	}

	head, _ := node.HeadBlock(ctx)
	fmt.Printf("\nregistered 3 assets in HintRegistry; head is block %d\n", head)

	if outPath != "" {
		if err := writeConfig(outPath, dsn, listen, nodeURL, chainID, registry, publisherKey); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", outPath)
		fmt.Printf("\nnext:  ./bin/evmscand -config %s\n", outPath)
	}
	return nil
}

// --------------------------------------------------------------------------
// Deploy / send helpers
// --------------------------------------------------------------------------

type wallet struct {
	key   *ecdsa.PrivateKey
	addr  common.Address
	nonce uint64
}

func (w *wallet) load(ctx context.Context, node *chain.Node) error {
	n, err := node.PendingNonceAt(ctx, w.addr)
	if err != nil {
		return err
	}
	w.nonce = n
	return nil
}

type deployer struct {
	node    *chain.Node
	chainID *big.Int
	faucet  common.Address
	last    common.Hash
}

// sendFaucet sends from the node's unlocked dev account.
func (d *deployer) sendFaucet(ctx context.Context, to *common.Address, value *big.Int, data []byte, gas uint64) (common.Hash, error) {
	h, err := d.node.SendUnsigned(ctx, d.faucet, to, value, data, gas)
	if err != nil {
		return common.Hash{}, err
	}
	d.last = h
	return h, nil
}

// sendFrom signs locally with a demo user's key.
func (d *deployer) sendFrom(ctx context.Context, w *wallet, to *common.Address, value *big.Int, data []byte, gas uint64) (common.Hash, error) {
	tip, err := d.node.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9)
	}
	head, err := d.node.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, err
	}
	feeCap := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: d.chainID, Nonce: w.nonce, GasTipCap: tip, GasFeeCap: feeCap,
		Gas: gas, To: to, Value: value, Data: data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(d.chainID), w.key)
	if err != nil {
		return common.Hash{}, err
	}
	if err := d.node.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, err
	}
	w.nonce++
	d.last = signed.Hash()
	return signed.Hash(), nil
}

// deploy sends creation bytecode and returns the deployed address.
func (d *deployer) deploy(ctx context.Context, name string, args ...any) (common.Address, error) {
	art, err := contracts.Load(name)
	if err != nil {
		return common.Address{}, err
	}
	parsed, err := art.Parsed()
	if err != nil {
		return common.Address{}, err
	}
	packed, err := parsed.Pack("", args...)
	if err != nil {
		return common.Address{}, fmt.Errorf("pack %s constructor: %w", name, err)
	}

	h, err := d.sendFaucet(ctx, nil, nil, append(art.Creation(), packed...), deployGas)
	if err != nil {
		return common.Address{}, err
	}
	r, err := d.receipt(ctx, h)
	if err != nil {
		return common.Address{}, err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return common.Address{}, fmt.Errorf("%s deployment reverted", name)
	}
	return r.ContractAddress, nil
}

// wait blocks until the most recent transaction has been mined, which is enough to
// order the phases: transactions from one sender are included in nonce order.
func (d *deployer) wait(ctx context.Context) error {
	if d.last == (common.Hash{}) {
		return nil
	}
	r, err := d.receipt(ctx, d.last)
	if err != nil {
		return err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("transaction %s reverted", d.last.Hex())
	}
	return nil
}

func (d *deployer) receipt(ctx context.Context, h common.Hash) (*types.Receipt, error) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	deadline := time.Now().Add(90 * time.Second)

	for {
		if r, err := d.node.TransactionReceipt(ctx, h); err == nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %s", h.Hex())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func abiOf(name string) (parsed abiLike, err error) {
	art, err := contracts.Load(name)
	if err != nil {
		return parsed, err
	}
	return art.Parsed()
}

// abiLike keeps the local signature short.
type abiLike = interface {
	Pack(name string, args ...any) ([]byte, error)
}

func ether(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18))
}

// deriveKey turns a label into a deterministic demo key.
func deriveKey(label string) (*ecdsa.PrivateKey, error) {
	return crypto.ToECDSA(crypto.Keccak256([]byte(label)))
}

func writeConfig(path, dsn, listen, node string, chainID uint64, registry common.Address, key *ecdsa.PrivateKey) error {
	body := fmt.Sprintf(`# Generated by evmscan-demo. Local dev chain only.
database:
  dsn: %q
  auto_migrate: true

api:
  listen: %q
  cors_origin: "*"
  # The on-chain HintRegistry is the real way in; this opens the local shortcut so
  # the demo UI can register an asset without sending a transaction.
  allow_registration: true

registry:
  chain_id: %d
  address: %q
  # Dev-chain key derived from a fixed seed. Never reuse this anywhere real.
  publisher_key: %q
  sync_interval: 5s
  auto_publish_interval: 0
  commitment_uri: ""

chains:
  - chain_id: %d
    name: devnet
    node: %q
    require_local_node: true
    # A dev chain has instant finality, so nothing needs to wait for confirmations.
    # On a real network set this to your reorg tolerance (12 is a common choice).
    confirmations: 0
    backfill_window: 5000
    tail_window: 1000
    poll_interval: 2s
    backfill_interval: 1s

    # Watch the head for contracts nobody registered. dSTEALTH above is deployed but
    # never registered, so it should show up under /v1/candidates.
    discovery:
      enabled: true
      lookback: 100000
      max_blocks_per_tick: 5000
      interval: 3s
      # Low thresholds so the demo promotes something within a minute. Production
      # wants these far higher, or auto_promote off entirely.
      auto_promote: true
      min_events: 10
      min_blocks: 3
      max_promotions_per_tick: 5
`, dsn, listen, chainID, registry.Hex(),
		fmt.Sprintf("0x%x", crypto.FromECDSA(key)), chainID, node)

	return os.WriteFile(path, []byte(body), 0o600)
}
