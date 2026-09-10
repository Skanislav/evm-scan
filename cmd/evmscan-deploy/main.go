// Command evmscan-deploy puts a HintRegistry on a real network and acts on it.
//
// evmscan-demo only works against `geth --dev`, because it relies on the node's
// unlocked account. This tool signs locally, so it works over any RPC endpoint, and
// it does not insist on a local node: deploying is a one-off, and the indexer's
// locality guarantee is about the node it reads from, not this.
//
// A registry is deployed in one of two adjudication modes, and the mode cannot be
// changed afterwards, so the choice is explicit and the tool reads it back off the
// chain and prints it:
//
//	optimistic-oracle  -oracle 0x… -bond-currency 0x…
//	                   Disputes go to UMA's Optimistic Oracle V3. No arbiter, no owner,
//	                   no admin setter is reachable — economics and gateways are final.
//
//	local-arbiter      -arbiter 0x…
//	                   The fallback for a chain with no oracle deployment: one key
//	                   settles disputes. The tool says so loudly, because a deployment
//	                   in this mode is only as neutral as that key.
//
// Usage:
//
//	evmscan-deploy -node https://... -key 0x... -arbiter 0x... \
//	    -publisher-bond 0 -asset-bond 0 -min-funding 1000000000000000 \
//	    -reward-per-block 100000000000 -challenge-window 3600 \
//	    -gateway 'https://host/ccip/{sender}/{data}.json'
//
//	evmscan-deploy -node https://... -key 0x... -oracle 0x... -bond-currency 0x... \
//	    -publisher-bond 500000000000000000 -challenge-window 7200
//
//	evmscan-deploy -node https://... -key 0x... -registry 0x... \
//	    -request 0xToken:20:8000000:5000000000000000
//
//	evmscan-deploy -node https://... -key 0x... -registry 0x... -fund 0xToken:1000000000000000
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/hintreg"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

type opts struct {
	node, key, registry string
	requireLocal        bool
	oracle, currency    string
	arbiter             string
	econ                hintreg.Economics
	timeout             time.Duration
	requests, funds     []string
	gateways            []string
}

func main() {
	var (
		o        opts
		requests listFlag
		funds    listFlag
		gateways listFlag
		window   uint64
	)
	flag.StringVar(&o.node, "node", "", "RPC endpoint of the target chain (ipc path, ws:// or http://)")
	flag.BoolVar(&o.requireLocal, "require-local-node", false, "refuse to deploy through a non-loopback endpoint")
	flag.StringVar(&o.key, "key", os.Getenv("EVMSCAN_DEPLOYER_KEY"), "hex private key that pays for everything (or EVMSCAN_DEPLOYER_KEY)")
	flag.StringVar(&o.registry, "registry", "", "existing HintRegistry to act on; empty deploys a new one")
	flag.StringVar(&o.oracle, "oracle", "", "UMA Optimistic Oracle V3 address; empty selects local-arbiter mode")
	flag.StringVar(&o.currency, "bond-currency", "", "ERC-20 the oracle bonds are denominated in (required with -oracle)")
	flag.StringVar(&o.arbiter, "arbiter", "", "dispute arbiter for a new registry (required without -oracle)")
	assetBond := flag.String("asset-bond", "0", "bond registerAsset requires, in wei")
	pubBond := flag.String("publisher-bond", "0", "bond publishIndex requires: bond-currency units with -oracle, wei without")
	flag.Uint64Var(&window, "challenge-window", 3600, "dispute window in seconds (assertion liveness with -oracle)")
	minFund := flag.String("min-funding", "0", "minimum requestIndexing deposit above the bond, in wei")
	reward := flag.String("reward-per-block", "0", "paid to a publisher per newly covered block of a funded asset, in wei")
	flag.DurationVar(&o.timeout, "timeout", 3*time.Minute, "how long to wait for each receipt")
	flag.Var(&requests, "request", "requestIndexing as token:kind:fromBlock:valueWei (repeatable)")
	flag.Var(&funds, "fund", "fundAsset for a token on this chain as token:valueWei (repeatable)")
	flag.Var(&gateways, "gateway", "ERC-3668 gateway URL template for contractsOf, e.g. https://host/ccip/{sender}/{data}.json (repeatable; set at deployment, or replaces the list on an existing local-arbiter registry)")
	flag.Parse()

	if o.node == "" {
		log.Fatal("-node is required")
	}
	// Inspecting an existing registry only reads, so it does not need a key. Anything
	// that sends a transaction does.
	sends := o.registry == "" || len(requests) > 0 || len(funds) > 0 || len(gateways) > 0
	if sends && o.key == "" {
		log.Fatal("-key (or EVMSCAN_DEPLOYER_KEY) is required to deploy or send; " +
			"pass only -node and -registry to inspect one")
	}
	if window == 0 {
		log.Fatal("-challenge-window must be greater than zero")
	}
	o.econ = hintreg.Economics{
		AssetBond:       mustWei(*assetBond),
		PublisherBond:   mustWei(*pubBond),
		ChallengeWindow: new(big.Int).SetUint64(window),
		MinFunding:      mustWei(*minFund),
		RewardPerBlock:  mustWei(*reward),
	}
	o.requests, o.funds, o.gateways = requests, funds, gateways

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := run(ctx, o); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, o opts) error {
	node, err := chain.Dial(ctx, o.node, o.requireLocal)
	if err != nil {
		return err
	}
	defer node.Close()

	chainID, err := node.ChainID(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("chain id   %d\n", chainID)

	var sub *hintreg.EOASubmitter
	if o.key != "" {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(o.key, "0x"))
		if err != nil {
			return fmt.Errorf("parse key: %w", err)
		}
		sub = hintreg.NewEOASubmitter(node, key, chainID, o.timeout)
		fmt.Printf("deployer   %s\n", sub.Sender().Hex())
	}

	art, err := contracts.Load("HintRegistry")
	if err != nil {
		return err
	}
	regABI, err := art.Parsed()
	if err != nil {
		return err
	}

	var registry common.Address
	if o.registry == "" {
		args, err := o.constructorArgs()
		if err != nil {
			return err
		}
		packed, err := regABI.Pack("", args...)
		if err != nil {
			return fmt.Errorf("pack constructor: %w", err)
		}
		h, err := sub.Deploy(ctx, append(art.Creation(), packed...))
		if err != nil {
			return err
		}
		fmt.Printf("deploy tx  %s\n", h.Hex())
		r, err := sub.Wait(ctx, h)
		if err != nil {
			return err
		}
		if r.Status != types.ReceiptStatusSuccessful {
			return errors.New("HintRegistry deployment reverted (bad mode combination or bond below the oracle's minimum?)")
		}
		registry = r.ContractAddress
		fmt.Printf("registry   %s\n\n", registry.Hex())

		// A receipt says the chain accepted the deployment; it does not say this
		// endpoint can see it yet. Behind a load balancer the next eth_call may land
		// on a node that has not imported the block, which answers a getter with
		// empty data and makes a perfectly good registry look broken.
		if err := awaitCode(ctx, node, registry); err != nil {
			fmt.Printf("\nnote: %v\n", err)
		}

		// Read the mode back off the chain rather than echoing the flags: what matters
		// is what the deployed bytecode says, not what we asked for.
		client, err := hintreg.NewClient(node, registry)
		if err != nil {
			return err
		}
		mode, err := client.Mode(ctx)
		if err != nil {
			// The registry exists — the receipt was successful and its address is
			// printed above. Failing here would tell an operator to deploy again,
			// which spends gas to produce a second registry that nothing points at.
			// Report it as what it is: a read that did not work yet.
			fmt.Printf("\nDEPLOYED. Could not read the registry back yet: %v\n", err)
			fmt.Printf("This is the endpoint lagging, not a failed deployment.\n")
			fmt.Printf("Do NOT deploy again. Use the address above and check it with:\n")
			fmt.Printf("  ./bin/evmscan-deploy -node <rpc> -registry %s\n", registry.Hex())
			return nil
		}
		printMode(mode)
		fmt.Printf("min funding      %s wei\n", o.econ.MinFunding)
		fmt.Printf("reward per block %s wei\n", o.econ.RewardPerBlock)
		fmt.Printf("gateways         %d\n", len(o.gateways))
	} else {
		if !common.IsHexAddress(o.registry) {
			return fmt.Errorf("bad registry address %q", o.registry)
		}
		registry = common.HexToAddress(o.registry)
		fmt.Printf("registry   %s\n", registry.Hex())

		if len(o.gateways) > 0 {
			data, err := regABI.Pack("setGateways", o.gateways)
			if err != nil {
				return fmt.Errorf("pack setGateways: %w", err)
			}
			if err := send(ctx, sub, registry, nil, data, fmt.Sprintf("setGateways (%d)", len(o.gateways))); err != nil {
				return fmt.Errorf("%w (only the arbiter of a local-arbiter registry may do this; in oracle mode the list is fixed at deployment)", err)
			}
		}
	}

	for _, spec := range o.requests {
		token, kind, from, value, err := parseRequest(spec)
		if err != nil {
			return err
		}
		data, err := regABI.Pack("requestIndexing", chainID, token, kind, from)
		if err != nil {
			return fmt.Errorf("pack requestIndexing: %w", err)
		}
		if err := send(ctx, sub, registry, value, data, "requestIndexing "+token.Hex()); err != nil {
			return err
		}
	}

	for _, spec := range o.funds {
		parts := strings.Split(spec, ":")
		if len(parts) != 2 || !common.IsHexAddress(parts[0]) {
			return fmt.Errorf("bad -fund %q, want token:valueWei", spec)
		}
		token := common.HexToAddress(parts[0])
		value, ok := new(big.Int).SetString(parts[1], 10)
		if !ok {
			return fmt.Errorf("bad -fund value %q", parts[1])
		}
		data, err := regABI.Pack("fundAsset", [32]byte(hintreg.AssetKey(chainID, token)))
		if err != nil {
			return fmt.Errorf("pack fundAsset: %w", err)
		}
		if err := send(ctx, sub, registry, value, data, "fundAsset "+token.Hex()); err != nil {
			return err
		}
	}

	fmt.Printf("\nnext:  EVMSCAN_REGISTRY_ADDRESS=%s\n", registry.Hex())
	return nil
}

// printMode is the whole point of deploying through this tool: a deployment's
// adjudication mode is fixed forever at construction, so it gets stated in full, once,
// at the moment it is fixed.
func printMode(m hintreg.Mode) {
	if m.OracleMode() {
		fmt.Printf("mode             optimistic-oracle\n")
		fmt.Printf("oracle           %s\n", m.Oracle.Hex())
		fmt.Printf("bond currency    %s\n", m.BondCurrency.Hex())
		fmt.Printf("publisher bond   %s (bond-currency units)\n", m.PublisherBond)
		fmt.Printf("asset bond       %s wei\n", m.AssetBond)
		fmt.Printf("liveness         %ds\n", m.ChallengeWindow)
		fmt.Printf("arbiter          none — disputes are settled by the oracle, and no admin\n")
		fmt.Printf("                 setter on this deployment is reachable.\n")
		return
	}

	fmt.Printf("mode             local-arbiter  (FALLBACK)\n")
	fmt.Printf("arbiter          %s\n", m.Arbiter.Hex())
	fmt.Printf("publisher bond   %s wei\n", m.PublisherBond)
	fmt.Printf("asset bond       %s wei\n", m.AssetBond)
	fmt.Printf("window           %ds\n", m.ChallengeWindow)
	fmt.Printf("\nWARNING: this deployment settles every dispute with one key. The bonds,\n")
	fmt.Printf("window and pricing above are immutable and the key cannot be reassigned, but\n")
	fmt.Printf("whoever holds it decides every challenge. Use it only on a chain with no\n")
	fmt.Printf("optimistic oracle deployment; pass -oracle and -bond-currency otherwise.\n")
	if m.PublisherBond.Sign() == 0 {
		fmt.Printf("\nWARNING: publisher bond is 0, so challengeIndex is free: anyone can park every\n")
		fmt.Printf("epoch in Challenged for the arbiter to clear. Fine for a demo, not for a\n")
		fmt.Printf("deployment anyone relies on. It cannot be raised later.\n")
	}
	fmt.Println()
}

// constructorArgs validates the flag combination and lays out HintRegistry's
// constructor. The contract enforces the same rules; failing here makes the error
// readable instead of a reverted deployment.
func (o opts) constructorArgs() ([]any, error) {
	oracle, err := optionalAddress("-oracle", o.oracle)
	if err != nil {
		return nil, err
	}
	currency, err := optionalAddress("-bond-currency", o.currency)
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
	return hintreg.ConstructorArgs(oracle, currency, arbiter, o.econ, o.gateways), nil
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

func send(ctx context.Context, sub *hintreg.EOASubmitter, to common.Address, value *big.Int, data []byte, what string) error {
	h, err := sub.Submit(ctx, to, value, data)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	r, err := sub.Wait(ctx, h)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("%s reverted (tx %s)", what, h.Hex())
	}
	fmt.Printf("%-40s tx %s  value %s wei\n", what, h.Hex(), value)
	return nil
}

// parseRequest reads token:kind:fromBlock:valueWei.
func parseRequest(spec string) (common.Address, uint8, uint64, *big.Int, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return common.Address{}, 0, 0, nil, fmt.Errorf("bad -request %q, want token:kind:fromBlock:valueWei", spec)
	}
	if !common.IsHexAddress(parts[0]) {
		return common.Address{}, 0, 0, nil, fmt.Errorf("bad -request token %q", parts[0])
	}
	kind, err := strconv.ParseUint(parts[1], 10, 8)
	if err != nil {
		return common.Address{}, 0, 0, nil, fmt.Errorf("bad -request kind %q (20, 21 or 55)", parts[1])
	}
	from, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return common.Address{}, 0, 0, nil, fmt.Errorf("bad -request fromBlock %q", parts[2])
	}
	value, ok := new(big.Int).SetString(parts[3], 10)
	if !ok {
		return common.Address{}, 0, 0, nil, fmt.Errorf("bad -request value %q", parts[3])
	}
	return common.HexToAddress(parts[0]), uint8(kind), from, value, nil
}

func mustWei(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 {
		log.Fatalf("bad wei amount %q", s)
	}
	return v
}

// awaitCode waits until the endpoint can actually see the deployed bytecode. A
// receipt only proves the chain accepted the transaction; a load-balanced RPC can
// still route the next call to a node a block or two behind, and eth_call against a
// block where the contract does not exist returns empty data rather than an error.
func awaitCode(ctx context.Context, src chain.Source, addr common.Address) error {
	const (
		attempts = 20
		wait     = 1500 * time.Millisecond
	)
	for i := 0; i < attempts; i++ {
		code, err := src.CodeAt(ctx, addr)
		if err == nil && len(code) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("%s still has no code after %s; the endpoint is behind the chain",
		addr.Hex(), time.Duration(attempts)*wait)
}
