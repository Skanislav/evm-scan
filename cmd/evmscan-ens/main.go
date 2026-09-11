// Command evmscan-ens hangs a HintResolver under an ENSv2 name, so the committed
// index is readable as ENS records: <hex-address>.hints.<yourname>.eth.
//
// Every ENSv2 address is a flag. The beta is explicitly unfinished and its
// deployments move, so nothing here is baked in except the canonical Universal
// Resolver address, and every contract is probed before a transaction is sent to it.
//
// Usage:
//
//	evmscan-ens deploy-resolver -node https://… -key 0x… -registry 0x<HintRegistry> [-chain-id 11155111]
//
//	evmscan-ens attach -node https://… -key 0x… -name yourname.eth -resolver 0x<HintResolver> \
//	    -eth-registry 0x… -factory 0x… -impl 0x… [-label hints] [-expiry <unix>]
//
//	evmscan-ens check -node https://… -name <hex>.hints.yourname.eth [-universal-resolver 0x…] [-registry 0x…]
//
//	evmscan-ens send -node https://… -key 0x… -tx '{"to":"0x…","data":"0x…","value":"0"}'
//
// `attach` is idempotent: it reuses the name's subregistry when it has one, deploys a
// UserRegistry through the VerifiableFactory when it does not (at the salt ens-cli
// would use, so either tool finds the same address), and registers or re-points the
// `hints` label. `send` signs and broadcasts the unsigned calldata ens-cli prints, so
// the two tools can be mixed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/ccip"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/ens"
	"github.com/Skanislav/evm-scan/internal/hintreg"
)

const canonicalUR = "0xeEeEEEeE14D718C2B47D9923Deab1335E144EeEe"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var err error
	switch cmd {
	case "deploy-resolver":
		err = deployResolver(ctx, args)
	case "attach":
		err = attach(ctx, args)
	case "check":
		err = check(ctx, args)
	case "send":
		err = send(ctx, args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: evmscan-ens <deploy-resolver|attach|check|send> [flags]

  deploy-resolver  deploy HintResolver bound to a HintRegistry
  attach           hang a HintResolver under <label>.<name> in ENSv2
  check            read-only: does <hex>.hints.<name> reach the resolver through the Universal Resolver?
  send             sign and broadcast unsigned calldata (e.g. from ens-cli --json)

Run a subcommand with -h for its flags.`)
}

// common holds the flags every subcommand shares.
type common_ struct {
	node    string
	key     string
	timeout time.Duration
}

func (c *common_) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.node, "node", "", "RPC endpoint (ipc path, ws:// or http://)")
	fs.StringVar(&c.key, "key", os.Getenv("EVMSCAN_DEPLOYER_KEY"), "hex private key that pays (or EVMSCAN_DEPLOYER_KEY)")
	fs.DurationVar(&c.timeout, "timeout", 3*time.Minute, "how long to wait for each receipt")
}

type session struct {
	node    *chain.Node
	chainID uint64
	sub     *hintreg.EOASubmitter
	call    ccip.Caller
}

func (c *common_) open(ctx context.Context, needKey bool) (*session, error) {
	if c.node == "" {
		return nil, errors.New("-node is required")
	}
	if needKey && c.key == "" {
		return nil, errors.New("-key (or EVMSCAN_DEPLOYER_KEY) is required to send")
	}
	node, err := chain.Dial(ctx, c.node, false)
	if err != nil {
		return nil, err
	}
	chainID, err := node.ChainID(ctx)
	if err != nil {
		node.Close()
		return nil, err
	}
	s := &session{node: node, chainID: chainID}
	s.call = func(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
		return node.CallAtHead(ctx, ethereum.CallMsg{To: &to, Data: data})
	}
	fmt.Printf("chain id   %d\n", chainID)
	if c.key != "" {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(c.key, "0x"))
		if err != nil {
			node.Close()
			return nil, fmt.Errorf("parse key: %w", err)
		}
		s.sub = hintreg.NewEOASubmitter(node, key, chainID, c.timeout)
		fmt.Printf("sender     %s\n", s.sub.Sender().Hex())
	}
	return s, nil
}

func (s *session) close() { s.node.Close() }

// mustHaveCode refuses to talk to an address nothing lives at. On a beta network
// whose deployments move, this is the difference between a clear error and a
// transaction that burns gas reverting.
func (s *session) mustHaveCode(ctx context.Context, what string, addr common.Address) error {
	code, err := s.node.CodeAt(ctx, addr)
	if err != nil {
		return fmt.Errorf("%s %s: read code: %w", what, addr.Hex(), err)
	}
	if len(code) == 0 {
		return fmt.Errorf("%s %s has no code on chain %d; check the address against docs.ens.domains/learn/deployments", what, addr.Hex(), s.chainID)
	}
	return nil
}

func (s *session) send(ctx context.Context, to common.Address, value *big.Int, data []byte, what string) (*types.Receipt, error) {
	h, err := s.sub.Submit(ctx, to, value, data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	r, err := s.sub.Wait(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return nil, fmt.Errorf("%s reverted (tx %s)", what, h.Hex())
	}
	fmt.Printf("%-44s tx %s\n", what, h.Hex())
	return r, nil
}

func address(flagName, raw string) (common.Address, error) {
	if !common.IsHexAddress(raw) {
		return common.Address{}, fmt.Errorf("%s %q is not an address", flagName, raw)
	}
	return common.HexToAddress(raw), nil
}

// ---------------------------------------------------------------- deploy-resolver

func deployResolver(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy-resolver", flag.ExitOnError)
	var c common_
	c.bind(fs)
	registryHex := fs.String("registry", "", "HintRegistry the resolver answers from")
	defaultChain := fs.Uint64("chain-id", 0, "chain the index is about (default: the node's own chain)")
	_ = fs.Parse(args)

	registry, err := address("-registry", *registryHex)
	if err != nil {
		return err
	}
	s, err := c.open(ctx, true)
	if err != nil {
		return err
	}
	defer s.close()
	if *defaultChain == 0 {
		*defaultChain = s.chainID
	}
	if err := s.mustHaveCode(ctx, "HintRegistry", registry); err != nil {
		return err
	}
	// Reading gateways() proves it is a HintRegistry and shows what the resolver
	// will advertise.
	client, err := hintreg.NewClient(s.node, registry)
	if err != nil {
		return err
	}
	urls, err := client.Gateways(ctx)
	if err != nil {
		return fmt.Errorf("%s does not answer gateways(); is it a HintRegistry? %w", registry.Hex(), err)
	}
	if len(urls) == 0 {
		fmt.Println("note: the registry advertises no gateways yet; evmscan.contracts will not resolve until it does")
	}

	art, err := contracts.Load("HintResolver")
	if err != nil {
		return err
	}
	resABI, err := art.Parsed()
	if err != nil {
		return err
	}
	ctor, err := resABI.Pack("", registry, *defaultChain)
	if err != nil {
		return err
	}
	h, err := s.sub.Deploy(ctx, append(art.Creation(), ctor...))
	if err != nil {
		return err
	}
	fmt.Printf("deploy tx  %s\n", h.Hex())
	r, err := s.sub.Wait(ctx, h)
	if err != nil {
		return err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return errors.New("HintResolver deployment reverted")
	}
	resolver := r.ContractAddress
	fmt.Printf("resolver   %s\n", resolver.Hex())

	// Read it back rather than echoing the flags.
	if out, err := s.call(ctx, resolver, resABI.Methods["registry"].ID); err == nil {
		if vals, err := resABI.Unpack("registry", out); err == nil {
			fmt.Printf("registry() %s\n", vals[0].(common.Address).Hex())
		}
	}
	if out, err := s.call(ctx, resolver, resABI.Methods["defaultChainId"].ID); err == nil {
		if vals, err := resABI.Unpack("defaultChainId", out); err == nil {
			fmt.Printf("chain      %d\n", vals[0].(uint64))
		}
	}
	fmt.Printf("gateways   %v\n", urls)
	fmt.Printf("\nnext:  evmscan-ens attach -node <rpc> -key <key> -name <yourname>.eth -resolver %s -eth-registry 0x… -factory 0x… -impl 0x…\n", resolver.Hex())
	return nil
}

// ------------------------------------------------------------------------ attach

func attach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	var c common_
	c.bind(fs)
	name := fs.String("name", "", "the .eth name you own on ENSv2 (e.g. yourname.eth)")
	resolverHex := fs.String("resolver", "", "HintResolver address")
	ethRegistryHex := fs.String("eth-registry", "", "ENSv2 ETHRegistry")
	factoryHex := fs.String("factory", "", "ENSv2 VerifiableFactory (needed only when the name has no subregistry yet)")
	implHex := fs.String("impl", "", "ENSv2 UserRegistry implementation (needed only when the name has no subregistry yet)")
	label := fs.String("label", "hints", "label to bind the resolver under")
	expiry := fs.Uint64("expiry", 0, "unix expiry for the label (default: 100 years from now)")
	_ = fs.Parse(args)

	if *name == "" || !strings.HasSuffix(*name, ".eth") || strings.Count(*name, ".") != 1 {
		return errors.New("-name must be a second-level .eth name you own, e.g. yourname.eth")
	}
	norm, err := ens.Normalize(*name)
	if err != nil {
		return err
	}
	resolver, err := address("-resolver", *resolverHex)
	if err != nil {
		return err
	}
	ethRegistry, err := address("-eth-registry", *ethRegistryHex)
	if err != nil {
		return err
	}
	s, err := c.open(ctx, true)
	if err != nil {
		return err
	}
	defer s.close()

	for what, a := range map[string]common.Address{"HintResolver": resolver, "ETHRegistry": ethRegistry} {
		if err := s.mustHaveCode(ctx, what, a); err != nil {
			return err
		}
	}
	// The resolver must say it is an extended resolver, or the Universal Resolver
	// will refuse to use it for wildcard names.
	if ok, err := supports(ctx, s.call, resolver, [4]byte{0x90, 0x61, 0xb9, 0x23}); err != nil || !ok {
		return fmt.Errorf("%s does not declare IExtendedResolver (0x9061b923); is it a HintResolver? (%v)", resolver.Hex(), err)
	}

	secondLabel := strings.TrimSuffix(norm, ".eth")
	owner, err := registryOwner(ctx, s.call, ethRegistry, secondLabel)
	if err != nil {
		return fmt.Errorf("read owner of %s in ETHRegistry: %w", norm, err)
	}
	fmt.Printf("name       %s (owner %s)\n", norm, owner.Hex())
	if owner != s.sub.Sender() {
		return fmt.Errorf("%s is owned by %s, not the sender %s", norm, owner.Hex(), s.sub.Sender().Hex())
	}

	// 1. The name's subregistry, reused or created.
	sub, err := registryAddressView(ctx, s.call, ethRegistry, "getSubregistry", secondLabel)
	if err != nil {
		return err
	}
	if sub == (common.Address{}) {
		factory, err := address("-factory", *factoryHex)
		if err != nil {
			return fmt.Errorf("%s has no subregistry yet; %w", norm, err)
		}
		impl, err := address("-impl", *implHex)
		if err != nil {
			return fmt.Errorf("%s has no subregistry yet; %w", norm, err)
		}
		for what, a := range map[string]common.Address{"VerifiableFactory": factory, "UserRegistry implementation": impl} {
			if err := s.mustHaveCode(ctx, what, a); err != nil {
				return err
			}
		}
		salt, err := ens.UserRegistrySalt(norm)
		if err != nil {
			return err
		}
		init, err := ens.UserRegistryInitABI.Pack("initialize", s.sub.Sender(), ens.OwnerRoles())
		if err != nil {
			return err
		}
		data, err := ens.FactoryABI.Pack("deployProxy", impl, salt, init)
		if err != nil {
			return err
		}
		r, err := s.send(ctx, factory, nil, data, "deployProxy(UserRegistry)")
		if err != nil {
			return err
		}
		sub, err = proxyFromReceipt(r, factory)
		if err != nil {
			return err
		}
		fmt.Printf("subregistry %s (new)\n", sub.Hex())

		data, err = ens.RegistryABI.Pack("setSubregistry", ens.LabelID(secondLabel), sub)
		if err != nil {
			return err
		}
		if _, err := s.send(ctx, ethRegistry, nil, data, "ETHRegistry.setSubregistry("+secondLabel+")"); err != nil {
			return err
		}
	} else {
		fmt.Printf("subregistry %s (existing)\n", sub.Hex())
	}

	// 2. The label, registered or re-pointed.
	current, err := registryAddressView(ctx, s.call, sub, "getResolver", *label)
	if err != nil {
		return fmt.Errorf("read %s.%s in the subregistry: %w", *label, norm, err)
	}
	switch {
	case current == resolver:
		fmt.Printf("label      %s.%s already points at the resolver\n", *label, norm)
	case current != (common.Address{}):
		data, err := ens.RegistryABI.Pack("setResolver", ens.LabelID(*label), resolver)
		if err != nil {
			return err
		}
		if _, err := s.send(ctx, sub, nil, data, "setResolver("+*label+")"); err != nil {
			return err
		}
	default:
		exp := *expiry
		if exp == 0 {
			exp = uint64(time.Now().Add(100 * 365 * 24 * time.Hour).Unix())
		}
		data, err := ens.RegistryABI.Pack("register", *label, s.sub.Sender(), common.Address{}, resolver, ens.LabelRoles(), exp)
		if err != nil {
			return err
		}
		if _, err := s.send(ctx, sub, nil, data, "register("+*label+")"); err != nil {
			return err
		}
	}

	got, err := registryAddressView(ctx, s.call, sub, "getResolver", *label)
	if err != nil {
		return err
	}
	if got != resolver {
		return fmt.Errorf("after wiring, %s.%s resolves to %s, not %s", *label, norm, got.Hex(), resolver.Hex())
	}
	fmt.Printf("\n%s.%s -> HintResolver %s\n", *label, norm, resolver.Hex())
	fmt.Printf("try:   evmscan-ens check -node <rpc> -name <hex-address>.%s.%s\n", *label, norm)
	return nil
}

// ------------------------------------------------------------------------- check

func check(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var c common_
	c.bind(fs)
	name := fs.String("name", "", "a hint name, e.g. <hex-address>.hints.yourname.eth")
	urHex := fs.String("universal-resolver", canonicalUR, "Universal Resolver to ask")
	registryHex := fs.String("registry", "", "expected HintRegistry (optional cross-check)")
	_ = fs.Parse(args)

	if *name == "" {
		return errors.New("-name is required")
	}
	ur, err := address("-universal-resolver", *urHex)
	if err != nil {
		return err
	}
	s, err := c.open(ctx, false)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.mustHaveCode(ctx, "Universal Resolver", ur); err != nil {
		return err
	}

	resolver, node, offset, err := ens.FindResolver(ctx, s.call, ur, *name)
	if err != nil {
		return fmt.Errorf("findResolver: %w", err)
	}
	fmt.Printf("name       %s\n", *name)
	fmt.Printf("namehash   %s\n", node.Hex())
	if resolver == (common.Address{}) {
		return errors.New("no resolver on the path; is the label registered with a resolver in the name's subregistry?")
	}
	fmt.Printf("resolver   %s (found %d label(s) up: %s)\n", resolver.Hex(), offset,
		map[bool]string{true: "wildcard, as intended", false: "exact"}[offset > 0])
	if err := s.mustHaveCode(ctx, "resolver", resolver); err != nil {
		return err
	}
	if ok, err := supports(ctx, s.call, resolver, [4]byte{0x90, 0x61, 0xb9, 0x23}); err != nil || !ok {
		return fmt.Errorf("resolver does not declare IExtendedResolver, so the Universal Resolver will not use it for wildcard names (%v)", err)
	}

	art, err := contracts.Load("HintResolver")
	if err != nil {
		return err
	}
	resABI, err := art.Parsed()
	if err != nil {
		return err
	}
	out, err := s.call(ctx, resolver, resABI.Methods["registry"].ID)
	if err != nil {
		return fmt.Errorf("resolver has no registry(); is it a HintResolver? %w", err)
	}
	vals, err := resABI.Unpack("registry", out)
	if err != nil {
		return err
	}
	registry := vals[0].(common.Address)
	fmt.Printf("registry   %s\n", registry.Hex())
	if *registryHex != "" {
		want, err := address("-registry", *registryHex)
		if err != nil {
			return err
		}
		if want != registry {
			return fmt.Errorf("resolver is bound to %s, expected %s", registry.Hex(), want.Hex())
		}
	}

	for _, key := range []string{"evmscan.chain", "evmscan.epoch", "evmscan.range", "evmscan.registry"} {
		v, err := ens.ResolveText(ctx, s.call, resABI, resolver, *name, key, http.DefaultClient, nil)
		if err != nil {
			fmt.Printf("%-18s error: %v\n", key, err)
			continue
		}
		fmt.Printf("%-18s %q\n", key, v)
	}
	v, err := ens.ResolveText(ctx, s.call, resABI, resolver, *name, "evmscan.contracts", http.DefaultClient, nil)
	if err != nil {
		fmt.Printf("%-18s error: %v\n", "evmscan.contracts", err)
		fmt.Println("\n(the plain records work; the contracts record needs a finalized epoch and a reachable gateway)")
		return nil
	}
	list, err := ens.SplitContracts(v)
	if err != nil {
		return err
	}
	fmt.Printf("%-18s %d contract(s), verified on-chain against the latest finalized epoch\n", "evmscan.contracts", len(list))
	for _, a := range list {
		fmt.Printf("  %s\n", a.Hex())
	}
	return nil
}

// -------------------------------------------------------------------------- send

func send(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	var c common_
	c.bind(fs)
	tx := fs.String("tx", "", `unsigned transaction as JSON: {"to":"0x…","data":"0x…","value":"0"} (or - for stdin)`)
	_ = fs.Parse(args)
	if *tx == "" {
		return errors.New("-tx is required")
	}
	raw := []byte(*tx)
	if *tx == "-" {
		var err error
		if raw, err = readAll(os.Stdin); err != nil {
			return err
		}
	}
	var req struct {
		To    string `json:"to"`
		Data  string `json:"data"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		// ens-cli wraps calldata in an envelope; accept the first object that has to/data.
		var env map[string]json.RawMessage
		if err2 := json.Unmarshal(raw, &env); err2 != nil {
			return fmt.Errorf("parse -tx: %w", err)
		}
		found := false
		for _, v := range env {
			if json.Unmarshal(v, &req) == nil && req.To != "" && req.Data != "" {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("parse -tx: no object with to/data: %w", err)
		}
	}
	to, err := address("to", req.To)
	if err != nil {
		return err
	}
	data, err := hexutil.Decode(req.Data)
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	value := new(big.Int)
	if req.Value != "" {
		v, ok := new(big.Int).SetString(strings.TrimPrefix(req.Value, "0x"), map[bool]int{true: 16, false: 10}[strings.HasPrefix(req.Value, "0x")])
		if !ok {
			return fmt.Errorf("value %q is not a number", req.Value)
		}
		value = v
	}
	s, err := c.open(ctx, true)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.mustHaveCode(ctx, "target", to); err != nil {
		return err
	}
	_, err = s.send(ctx, to, value, data, fmt.Sprintf("send to %s (%s)", to.Hex(), ccip.Selector(data)))
	return err
}

// ------------------------------------------------------------------------ helpers

func supports(ctx context.Context, call ccip.Caller, at common.Address, iface [4]byte) (bool, error) {
	data, err := ens.RegistryABI.Pack("supportsInterface", iface)
	if err != nil {
		return false, err
	}
	out, err := call(ctx, at, data)
	if err != nil {
		return false, err
	}
	vals, err := ens.RegistryABI.Unpack("supportsInterface", out)
	if err != nil {
		return false, err
	}
	return vals[0].(bool), nil
}

func registryAddressView(ctx context.Context, call ccip.Caller, registry common.Address, method, label string) (common.Address, error) {
	data, err := ens.RegistryABI.Pack(method, label)
	if err != nil {
		return common.Address{}, err
	}
	out, err := call(ctx, registry, data)
	if err != nil {
		return common.Address{}, fmt.Errorf("%s(%q): %w", method, label, err)
	}
	vals, err := ens.RegistryABI.Unpack(method, out)
	if err != nil {
		return common.Address{}, err
	}
	return vals[0].(common.Address), nil
}

func registryOwner(ctx context.Context, call ccip.Caller, registry common.Address, label string) (common.Address, error) {
	data, err := ens.RegistryABI.Pack("getOwner", ens.LabelID(label))
	if err != nil {
		return common.Address{}, err
	}
	out, err := call(ctx, registry, data)
	if err != nil {
		return common.Address{}, err
	}
	vals, err := ens.RegistryABI.Unpack("getOwner", out)
	if err != nil {
		return common.Address{}, err
	}
	return vals[0].(common.Address), nil
}

// proxyFromReceipt reads the ProxyDeployed event the factory emits.
func proxyFromReceipt(r *types.Receipt, factory common.Address) (common.Address, error) {
	ev := ens.FactoryABI.Events["ProxyDeployed"]
	for _, l := range r.Logs {
		if l.Address != factory || len(l.Topics) < 3 || l.Topics[0] != ev.ID {
			continue
		}
		return common.BytesToAddress(l.Topics[2].Bytes()), nil
	}
	return common.Address{}, errors.New("deployProxy succeeded but no ProxyDeployed event was found in the receipt")
}

func readAll(f *os.File) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if errors.Is(err, os.ErrClosed) || err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
		if n == 0 {
			return out, nil
		}
	}
}
