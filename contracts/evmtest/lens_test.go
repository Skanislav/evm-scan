package evmtest_test

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/holiman/uint256"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/lens"
)

// TestLensAgainstEVM runs the compiled lens in a real EVM.
//
// Everything else in this package tests the wire format against a node that only
// pretends. This one settles the part that cannot be faked: that a client will
// execute creation code sent with no `to` address, hand back what the constructor
// returned, and that the contract reads real tokens correctly on the way.
func TestLensAgainstEVM(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	erc20 := h.deploy("DemoERC20", "Demo Dollar", "DUSD")
	erc721 := h.deploy("DemoERC721", "Demo Apes", "APE")
	spender := common.HexToAddress("0x00000000000000000000000000000000000000ff")

	h.send(erc20, "mint", h.from, big.NewInt(4_200))
	h.send(erc20, "approve", spender, big.NewInt(777))
	h.send(erc721, "mint", h.from)
	h.send(erc721, "mint", h.from)
	h.send(erc721, "setApprovalForAll", spender, true)

	res, err := lens.Query(context.Background(), h.src(), lens.Request{
		Account:  h.from,
		Spenders: []common.Address{spender},
		Tokens: []lens.TokenQuery{
			{Token: erc20},
			{Token: erc721, IDs: []*big.Int{big.NewInt(0), big.NewInt(1), big.NewInt(9)}},
			// An address with no code at all: the lens has to say so rather than
			// report a contract that answered nothing.
			{Token: common.HexToAddress("0xdead")},
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if res.Calls != 1 {
		t.Errorf("three tokens took %d calls", res.Calls)
	}
	if !res.Atomic {
		t.Error("a single call is atomic by construction")
	}
	if got, want := res.BlockNumber, h.head(); got != want {
		t.Errorf("block = %d, head = %d", got, want)
	}
	if res.ChainID != h.chainID.Uint64() {
		t.Errorf("chain id = %d, want %d", res.ChainID, h.chainID)
	}

	// The account: an EOA that has been paying for these transactions.
	if res.Account.Balance == nil || res.Account.Balance.Sign() <= 0 {
		t.Errorf("native balance = %v", res.Account.Balance)
	}
	// Seven transactions in: two deployments and five calls. The nonce is the one
	// fact here that no contract can report, so it has to line up with the RPC.
	if !res.Account.NonceKnown || res.Account.Nonce != 7 {
		t.Errorf("nonce = %d (known=%v), want 7", res.Account.Nonce, res.Account.NonceKnown)
	}
	if res.Account.IsContract || res.Account.IsDelegated {
		t.Errorf("EOA reported as contract=%v delegated=%v", res.Account.IsContract, res.Account.IsDelegated)
	}

	// ERC-20: no ERC-165, identified by shape.
	tok := res.Tokens[0]
	if tok.Address != erc20 || !tok.IsContract {
		t.Fatalf("token 0 = %+v", tok)
	}
	if tok.Standard != evmlog.StandardERC20 {
		t.Errorf("standard = %s, want erc20", tok.Standard)
	}
	if tok.Symbol != "DUSD" || tok.Name != "Demo Dollar" {
		t.Errorf("metadata = %q / %q", tok.Symbol, tok.Name)
	}
	if tok.Decimals == nil || *tok.Decimals != 18 {
		t.Errorf("decimals = %v", tok.Decimals)
	}
	if tok.Balance == nil || tok.Balance.Int64() != 4_200 {
		t.Errorf("balance = %v, want 4200", tok.Balance)
	}
	if tok.TotalSupply == nil || tok.TotalSupply.Int64() != 4_200 {
		t.Errorf("total supply = %v", tok.TotalSupply)
	}
	if len(tok.Allowances) != 1 || tok.Allowances[0].Amount == nil ||
		tok.Allowances[0].Amount.Int64() != 777 {
		t.Errorf("allowance = %+v", tok.Allowances)
	}

	// ERC-721: identified through ERC-165, with per-id ownership resolved.
	nft := res.Tokens[1]
	if nft.Standard != evmlog.StandardERC721 || !nft.SupportsERC165 {
		t.Errorf("standard = %s erc165 = %v", nft.Standard, nft.SupportsERC165)
	}
	if nft.Balance == nil || nft.Balance.Int64() != 2 {
		t.Errorf("held count = %v, want 2", nft.Balance)
	}
	if nft.Decimals != nil {
		t.Errorf("an ERC-721 has no decimals, got %v", *nft.Decimals)
	}
	if len(nft.Allowances) != 1 || !nft.Allowances[0].ApprovedForAll {
		t.Errorf("operator approval = %+v", nft.Allowances)
	}
	if nft.Allowances[0].Amount != nil {
		t.Errorf("an NFT has no allowance amount, got %v", nft.Allowances[0].Amount)
	}
	if len(nft.IDs) != 3 {
		t.Fatalf("got %d ids", len(nft.IDs))
	}
	for _, id := range nft.IDs[:2] {
		if id.Owner == nil || *id.Owner != h.from {
			t.Errorf("id %s owner = %v, want %s", id.ID, id.Owner, h.from)
		}
		if id.Balance == nil || id.Balance.Int64() != 1 {
			t.Errorf("id %s balance = %v", id.ID, id.Balance)
		}
	}
	// Id 9 was never minted. DemoERC721 stores owners in a mapping, so it answers
	// with the zero address instead of reverting; either way the account holds none
	// of it, and that is what must come back.
	if unminted := nft.IDs[2]; unminted.Balance != nil && unminted.Balance.Sign() != 0 {
		t.Errorf("unminted id reported balance %v", unminted.Balance)
	}

	// An address with no code is not a token, and nothing about it is knowable.
	empty := res.Tokens[2]
	if empty.IsContract || empty.Standard != evmlog.StandardUnknown || empty.Balance != nil {
		t.Errorf("empty address = %+v", empty)
	}
}

// TestLensSplitsRepliesThatExceedTheCodeSizeLimit is EIP-170 hitting a real client.
//
// The constructor's return value is treated as deployed code, so a portfolio that
// encodes to more than 24576 bytes does not come back truncated — the whole eth_call
// fails. A wallet with a few hundred tokens is an ordinary case, so the client has to
// discover the boundary and split, which is what this asserts against a node that
// really does enforce the rule.
func TestLensSplitsRepliesThatExceedTheCodeSizeLimit(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	erc20 := h.deploy("DemoERC20", "Demo Dollar With A Deliberately Long Name", "DUSD")
	h.send(erc20, "mint", h.from, big.NewInt(1))

	const n = 200
	req := lens.Request{Account: h.from, MaxStringBytes: 128}
	for range n {
		req.Tokens = append(req.Tokens, lens.TokenQuery{Token: erc20})
	}

	// Ask for all of it in one call, so the node has to be the one that says no and
	// the split is a recovery rather than a precaution.
	req.TokensPerCall = n
	res, err := lens.Query(context.Background(), h.src(), req)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Tokens) != n {
		t.Fatalf("got %d tokens, want %d", len(res.Tokens), n)
	}
	if res.Calls < 2 {
		t.Errorf("the node accepted %d tokens in one reply; expected it to be rejected and split", n)
	}
	if !res.Atomic {
		t.Error("no block was mined between calls; the snapshot is still atomic")
	}
	for i, tok := range res.Tokens {
		if tok.Balance == nil || tok.Balance.Int64() != 1 {
			t.Fatalf("token %d balance = %v", i, tok.Balance)
		}
	}
}

// TestLensSeesContractsAndDelegation covers the account half: code, its hash, and the
// EIP-7702 designator that makes an EOA execute somebody else's code. A wallet that
// cannot see a delegation cannot warn about one.
func TestLensSeesContractsAndDelegation(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	erc20 := h.deploy("DemoERC20", "Demo Dollar", "DUSD")

	res, err := lens.Query(context.Background(), h.src(), lens.Request{Account: erc20, IncludeCode: true})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !res.Account.IsContract || res.Account.IsDelegated {
		t.Errorf("contract account = %+v", res.Account)
	}
	code := h.code(erc20)
	if res.Account.CodeSize != uint64(len(code)) || res.Account.CodeHash != crypto.Keccak256Hash(code) {
		t.Errorf("code size %d hash %s, want %d / %s",
			res.Account.CodeSize, res.Account.CodeHash, len(code), crypto.Keccak256Hash(code))
	}
	if string(res.Account.Code) != string(code) {
		t.Error("returned code does not match the node's")
	}

	// Delegate the EOA to the token contract, EIP-7702 style.
	h.delegate(erc20)

	res, err = lens.Query(context.Background(), h.src(), lens.Request{Account: h.from})
	if err != nil {
		t.Fatalf("query after delegation: %v", err)
	}
	if !res.Account.IsDelegated || res.Account.Delegate != erc20 {
		t.Fatalf("delegation = %v -> %s, want true -> %s",
			res.Account.IsDelegated, res.Account.Delegate, erc20)
	}
	// A delegated EOA has code, but calling it is nothing like calling a contract,
	// so it must not be reported as one.
	if res.Account.IsContract {
		t.Error("a delegated EOA is not a contract")
	}
	if res.Account.CodeSize != 23 {
		t.Errorf("designator is %d bytes, want 23", res.Account.CodeSize)
	}
}

// --------------------------------------------------------------------------
// Harness: a real EVM, driven over the same Source interface the daemon uses
// --------------------------------------------------------------------------

type harness struct {
	t       *testing.T
	backend *simulated.Backend
	client  simulated.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int
	abis    map[string]abi.ABI
	// contractNames remembers what was deployed where, so send() can find the ABI.
	contractNames map[common.Address]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	backend := simulated.NewBackend(types.GenesisAlloc{
		from: {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
	})
	client := backend.Client()
	chainID, err := client.ChainID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		t: t, backend: backend, client: client, key: key, from: from,
		chainID: chainID, abis: map[string]abi.ABI{},
		contractNames: map[common.Address]string{},
	}
}

func (h *harness) close() { _ = h.backend.Close() }

func (h *harness) src() chain.Source { return simSource{h.client} }

func (h *harness) artifact(name string) (contracts.Artifact, abi.ABI) {
	h.t.Helper()
	a, err := contracts.Load(name)
	if err != nil {
		h.t.Fatal(err)
	}
	parsed, err := a.Parsed()
	if err != nil {
		h.t.Fatal(err)
	}
	h.abis[name] = parsed
	return a, parsed
}

func (h *harness) deploy(name string, args ...any) common.Address {
	h.t.Helper()
	a, parsed := h.artifact(name)
	packed, err := parsed.Pack("", args...)
	if err != nil {
		h.t.Fatalf("pack %s constructor: %v", name, err)
	}
	rcpt := h.tx(nil, append(a.Creation(), packed...))
	if rcpt.ContractAddress == (common.Address{}) {
		h.t.Fatalf("%s did not deploy", name)
	}
	h.contractNames[rcpt.ContractAddress] = name
	return rcpt.ContractAddress
}

func (h *harness) send(to common.Address, method string, args ...any) {
	h.t.Helper()
	parsed := h.abis[h.contractNames[to]]
	data, err := parsed.Pack(method, args...)
	if err != nil {
		h.t.Fatalf("pack %s: %v", method, err)
	}
	if rcpt := h.tx(&to, data); rcpt.Status != types.ReceiptStatusSuccessful {
		h.t.Fatalf("%s reverted", method)
	}
}

// delegate points the harness account's code at target, as EIP-7702 does.
func (h *harness) delegate(target common.Address) {
	h.t.Helper()
	ctx := context.Background()
	nonce, err := h.client.PendingNonceAt(ctx, h.from)
	if err != nil {
		h.t.Fatal(err)
	}
	auth, err := types.SignSetCode(h.key, types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(h.chainID),
		Address: target,
		// The authorization is consumed by the transaction that carries it, so it
		// authorises the nonce this account will have once that transaction lands.
		Nonce: nonce + 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	tip, feeCap := h.fees()
	tx := types.MustSignNewTx(h.key, types.LatestSignerForChainID(h.chainID), &types.SetCodeTx{
		ChainID:   uint256.MustFromBig(h.chainID),
		Nonce:     nonce,
		GasTipCap: uint256.MustFromBig(tip),
		GasFeeCap: uint256.MustFromBig(feeCap),
		Gas:       200_000,
		// Anywhere but the delegating account: sending to itself would execute the
		// freshly delegated code with empty calldata and revert on the way out.
		To:       common.Address{},
		AuthList: []types.SetCodeAuthorization{auth},
	})
	if err := h.client.SendTransaction(ctx, tx); err != nil {
		h.t.Fatalf("send 7702 tx: %v", err)
	}
	h.backend.Commit()
	rcpt, err := h.client.TransactionReceipt(ctx, tx.Hash())
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		h.t.Fatalf("7702 tx failed: %v", err)
	}
}

func (h *harness) tx(to *common.Address, data []byte) *types.Receipt {
	h.t.Helper()
	ctx := context.Background()
	nonce, err := h.client.PendingNonceAt(ctx, h.from)
	if err != nil {
		h.t.Fatal(err)
	}
	tip, feeCap := h.fees()
	tx := types.MustSignNewTx(h.key, types.LatestSignerForChainID(h.chainID), &types.DynamicFeeTx{
		ChainID:   h.chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       3_000_000,
		To:        to,
		Data:      data,
	})
	if err := h.client.SendTransaction(ctx, tx); err != nil {
		h.t.Fatalf("send tx: %v", err)
	}
	h.backend.Commit()
	rcpt, err := h.client.TransactionReceipt(ctx, tx.Hash())
	if err != nil {
		h.t.Fatalf("receipt: %v", err)
	}
	return rcpt
}

func (h *harness) fees() (tip, feeCap *big.Int) {
	h.t.Helper()
	head, err := h.client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	tip = big.NewInt(1e9)
	return tip, new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
}

func (h *harness) head() uint64 {
	h.t.Helper()
	n, err := h.client.BlockNumber(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) code(addr common.Address) []byte {
	h.t.Helper()
	code, err := h.client.CodeAt(context.Background(), addr, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return code
}

// simSource adapts the simulated client to the interface the daemon reads through,
// so this test exercises the same code path production does.
type simSource struct{ c simulated.Client }

func (s simSource) CallAtHead(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	return s.c.CallContract(ctx, msg, nil)
}
func (s simSource) NonceAt(ctx context.Context, addr common.Address) (uint64, error) {
	return s.c.NonceAt(ctx, addr, nil)
}
func (s simSource) CodeAt(ctx context.Context, addr common.Address) ([]byte, error) {
	return s.c.CodeAt(ctx, addr, nil)
}
func (s simSource) ChainID(ctx context.Context) (uint64, error) {
	id, err := s.c.ChainID(ctx)
	if err != nil {
		return 0, err
	}
	return id.Uint64(), nil
}
func (s simSource) HeadBlock(ctx context.Context) (uint64, error) { return s.c.BlockNumber(ctx) }
func (s simSource) HeaderHash(ctx context.Context, n uint64) (common.Hash, error) {
	h, err := s.c.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
	if err != nil {
		return common.Hash{}, err
	}
	return h.Hash(), nil
}
func (s simSource) Logs(context.Context, chain.Query) ([]types.Log, error) { return nil, nil }
func (s simSource) SubscribeLogs(context.Context, chain.Query, chan<- types.Log) (ethereum.Subscription, error) {
	return nil, chain.ErrNotStreaming
}
func (s simSource) Endpoint() chain.Endpoint { return chain.Endpoint{Raw: "simulated", Local: true} }
func (s simSource) Close()                   {}
