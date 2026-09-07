// Command evmscan-deploy deploys a HintRegistry and prints how it will settle disputes.
//
// A registry is deployed in one of two adjudication modes, and the mode cannot be
// changed afterwards, so this tool exists to make the choice explicit and to print it
// back:
//
//	optimistic-oracle  -oracle 0x… -bond-currency 0x…
//	                   Disputes go to UMA's Optimistic Oracle V3. No arbiter, no owner,
//	                   no admin setter is reachable.
//
//	local-arbiter      -arbiter 0x…
//	                   The fallback for a chain with no oracle deployment: one key
//	                   settles disputes. The tool says so loudly, because a deployment
//	                   in this mode is only as neutral as that key.
//
// Usage:
//
//	evmscan-deploy -node /tmp/devchain/geth.ipc -arbiter 0xabc… -challenge-window 3600
//	evmscan-deploy -node ws://127.0.0.1:8546 -key 0x… \
//	    -oracle 0x… -bond-currency 0x… -publisher-bond 500000000000000000
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/hintreg"
)

type options struct {
	nodeURL         string
	requireLocal    bool
	keyHex          string
	oracle          string
	bondCurrency    string
	arbiter         string
	assetBond       string
	publisherBond   string
	challengeWindow uint64
}

func main() {
	var o options
	flag.StringVar(&o.nodeURL, "node", "", "IPC path or ws/http URL of the node to deploy through")
	flag.BoolVar(&o.requireLocal, "require-local-node", true, "refuse to deploy through a non-loopback endpoint")
	flag.StringVar(&o.keyHex, "key", "", "deployer private key; omit to use the node's first unlocked account (dev chains)")
	flag.StringVar(&o.oracle, "oracle", "", "UMA Optimistic Oracle V3 address; empty selects local-arbiter mode")
	flag.StringVar(&o.bondCurrency, "bond-currency", "", "ERC-20 the oracle bonds are denominated in (required with -oracle)")
	flag.StringVar(&o.arbiter, "arbiter", "", "dispute arbiter (required without -oracle)")
	flag.StringVar(&o.assetBond, "asset-bond", "0", "bond an asset hint costs, in wei")
	flag.StringVar(&o.publisherBond, "publisher-bond", "0", "bond a commitment costs: bond-currency units with -oracle, wei without")
	flag.Uint64Var(&o.challengeWindow, "challenge-window", 7200, "dispute window in seconds (assertion liveness with -oracle)")
	flag.Parse()

	if o.nodeURL == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := run(ctx, o); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, o options) error {
	args, err := o.constructorArgs()
	if err != nil {
		return err
	}

	node, err := chain.Dial(ctx, o.nodeURL, o.requireLocal)
	if err != nil {
		return err
	}
	defer node.Close()

	chainID, err := node.ChainID(ctx)
	if err != nil {
		return err
	}

	d, err := newDeployer(ctx, node, chainID, o.keyHex)
	if err != nil {
		return err
	}

	fmt.Printf("chain id       %d\n", chainID)
	fmt.Printf("node           %s\n", node.Endpoint().String())
	fmt.Printf("deployer       %s\n", d.from.Hex())

	addr, err := d.deploy(ctx, "HintRegistry", args...)
	if err != nil {
		return fmt.Errorf("deploy HintRegistry: %w", err)
	}
	fmt.Printf("HintRegistry   %s\n\n", addr.Hex())

	// Read the mode back off the chain rather than echoing the flags: what matters is
	// what the deployed bytecode says, not what we asked for.
	client, err := hintreg.NewClient(node, addr)
	if err != nil {
		return err
	}
	mode, err := client.Mode(ctx)
	if err != nil {
		return err
	}
	printMode(mode)

	fmt.Printf("\nregistry:\n  chain_id: %d\n  address: %q\n", chainID, addr.Hex())
	return nil
}

// printMode is the whole point of the tool: a deployment's adjudication mode is fixed
// forever at construction, so it gets stated in full, once, at the moment it is fixed.
func printMode(m hintreg.Mode) {
	if m.OracleMode() {
		fmt.Printf("mode           optimistic-oracle\n")
		fmt.Printf("oracle         %s\n", m.Oracle.Hex())
		fmt.Printf("bond currency  %s\n", m.BondCurrency.Hex())
		fmt.Printf("publisher bond %s (bond-currency units)\n", m.PublisherBond)
		fmt.Printf("asset bond     %s wei\n", m.AssetBond)
		fmt.Printf("liveness       %ds\n", m.ChallengeWindow)
		fmt.Printf("arbiter        none — disputes are settled by the oracle, and no admin\n")
		fmt.Printf("               setter on this deployment is reachable.\n")
		return
	}

	fmt.Printf("mode           local-arbiter  (FALLBACK)\n")
	fmt.Printf("arbiter        %s\n", m.Arbiter.Hex())
	fmt.Printf("publisher bond %s wei\n", m.PublisherBond)
	fmt.Printf("asset bond     %s wei\n", m.AssetBond)
	fmt.Printf("window         %ds\n", m.ChallengeWindow)
	fmt.Printf("\nWARNING: this deployment settles every dispute with one key, which can\n")
	fmt.Printf("also retune the bonds and hand itself over. Use it only on a chain with no\n")
	fmt.Printf("optimistic oracle deployment; pass -oracle and -bond-currency otherwise.\n")
}

// constructorArgs validates the flag combination and packs HintRegistry's constructor.
// The contract enforces the same rules; failing here just makes the error readable.
func (o options) constructorArgs() ([]any, error) {
	assetBond, ok := new(big.Int).SetString(o.assetBond, 10)
	if !ok || assetBond.Sign() < 0 {
		return nil, fmt.Errorf("-asset-bond %q is not a non-negative integer", o.assetBond)
	}
	publisherBond, ok := new(big.Int).SetString(o.publisherBond, 10)
	if !ok || publisherBond.Sign() < 0 {
		return nil, fmt.Errorf("-publisher-bond %q is not a non-negative integer", o.publisherBond)
	}
	if o.challengeWindow == 0 {
		return nil, errors.New("-challenge-window must be greater than zero")
	}

	oracle, err := optionalAddress("-oracle", o.oracle)
	if err != nil {
		return nil, err
	}
	currency, err := optionalAddress("-bond-currency", o.bondCurrency)
	if err != nil {
		return nil, err
	}
	arbiter, err := optionalAddress("-arbiter", o.arbiter)
	if err != nil {
		return nil, err
	}

	zero := common.Address{}
	if oracle != zero {
		if currency == zero {
			return nil, errors.New("-oracle needs -bond-currency: oracle bonds are ERC-20, not ether")
		}
		if arbiter != zero {
			return nil, errors.New("-arbiter cannot be combined with -oracle: the oracle settles disputes")
		}
	} else {
		if arbiter == zero {
			return nil, errors.New("pass -oracle with -bond-currency, or -arbiter to fall back to a local arbiter")
		}
		if currency != zero {
			return nil, errors.New("-bond-currency only applies with -oracle; local-arbiter bonds are wei")
		}
	}

	return []any{oracle, currency, arbiter, assetBond, publisherBond, new(big.Int).SetUint64(o.challengeWindow)}, nil
}

func optionalAddress(flagName, raw string) (common.Address, error) {
	if raw == "" {
		return common.Address{}, nil
	}
	if !common.IsHexAddress(raw) {
		return common.Address{}, fmt.Errorf("%s %q is not an address", flagName, raw)
	}
	return common.HexToAddress(raw), nil
}

// --------------------------------------------------------------------------
// Deploy helper
// --------------------------------------------------------------------------

// deployer sends either locally signed transactions or, on a dev chain with an unlocked
// account, unsigned ones through the node.
type deployer struct {
	node    *chain.Node
	chainID *big.Int
	from    common.Address
	key     *ecdsa.PrivateKey
}

func newDeployer(ctx context.Context, node *chain.Node, chainID uint64, keyHex string) (*deployer, error) {
	d := &deployer{node: node, chainID: new(big.Int).SetUint64(chainID)}

	if keyHex != "" {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(keyHex, "0x"))
		if err != nil {
			return nil, fmt.Errorf("parse -key: %w", err)
		}
		d.key = key
		d.from = crypto.PubkeyToAddress(key.PublicKey)
		return d, nil
	}

	accounts, err := node.Accounts(ctx)
	if err != nil || len(accounts) == 0 {
		return nil, fmt.Errorf("no -key given and the node has no unlocked account: %w", err)
	}
	d.from = accounts[0]
	return d, nil
}

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
	code := append(art.Creation(), packed...)

	// Estimating doubles as a dry run: a constructor that reverts on a bad configuration
	// should not cost a deployment's worth of gas to discover.
	gas, err := d.node.EstimateGas(ctx, ethereum.CallMsg{From: d.from, Data: code})
	if err != nil {
		return common.Address{}, fmt.Errorf("%s constructor would revert: %w", name, err)
	}
	gas += gas / 5

	h, err := d.send(ctx, nil, code, gas)
	if err != nil {
		return common.Address{}, err
	}
	r, err := d.receipt(ctx, h)
	if err != nil {
		return common.Address{}, err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return common.Address{}, fmt.Errorf("%s deployment reverted (tx %s)", name, h.Hex())
	}
	return r.ContractAddress, nil
}

func (d *deployer) send(ctx context.Context, to *common.Address, data []byte, gas uint64) (common.Hash, error) {
	if d.key == nil {
		return d.node.SendUnsigned(ctx, d.from, to, nil, data, gas)
	}

	nonce, err := d.node.PendingNonceAt(ctx, d.from)
	if err != nil {
		return common.Hash{}, err
	}
	tip, err := d.node.SuggestGasTipCap(ctx)
	if err != nil {
		tip = big.NewInt(1e9)
	}
	head, err := d.node.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, err
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   d.chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2))),
		Gas:       gas,
		To:        to,
		Data:      data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(d.chainID), d.key)
	if err != nil {
		return common.Hash{}, err
	}
	if err := d.node.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, err
	}
	return signed.Hash(), nil
}

func (d *deployer) receipt(ctx context.Context, h common.Hash) (*types.Receipt, error) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	deadline := time.Now().Add(3 * time.Minute)

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
