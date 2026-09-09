package price

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
)

// The wire types below mirror the structs in contracts/src/PriceLens.sol under the
// same two rules internal/lens lives by: request fields are matched to ABI components
// by name (ToCamelCase of the Solidity name), reply fields are copied by position.
// TestWireMatchesABI walks the compiled artifact and asserts both.
//
// Integer widths follow go-ethereum's mapping: only 8/16/32/64-bit integers become
// native Go ints; everything else (uint24, int56, uint80, uint112, uint128, uint160,
// int256) arrives as *big.Int.

type wireFeedHint struct {
	Token      common.Address
	Aggregator common.Address
	Quote      uint8
}

type wireRequest struct {
	Tokens         []common.Address
	FeedRegistry   common.Address
	NativeUsdFeed  common.Address
	Feeds          []wireFeedHint
	V3Factory      common.Address
	FeeTiers       []*big.Int
	V2Factory      common.Address
	QuoteTokens    []common.Address
	TwapWindow     uint32
	GasPerCall     *big.Int
	MaxStringBytes *big.Int
}

type wireChainInfo struct {
	ChainId     *big.Int
	BlockNumber *big.Int
	ParentHash  common.Hash
	Timestamp   *big.Int
	BaseFee     *big.Int
}

type wireFeedInfo struct {
	Aggregator      common.Address
	Quote           uint8
	ViaRegistry     bool
	Ok              bool
	Answer          *big.Int
	Decimals        uint8
	HasDecimals     bool
	StartedAt       *big.Int
	UpdatedAt       *big.Int
	RoundId         *big.Int
	AnsweredInRound *big.Int
	Description     string
}

type wirePoolInfo struct {
	Pool                common.Address
	Kind                uint8
	Token0              common.Address
	Token1              common.Address
	Fee                 *big.Int
	SqrtPriceX96        *big.Int
	Tick                *big.Int
	Liquidity           *big.Int
	TwapWindow          uint32
	TickCumulativeStart *big.Int
	TickCumulativeEnd   *big.Int
	Reserve0            *big.Int
	Reserve1            *big.Int
	ReserveTimestamp    uint32
}

type wireTokenPrices struct {
	Token       common.Address
	IsContract  bool
	Symbol      string
	Decimals    uint8
	HasDecimals bool
	Feeds       []wireFeedInfo
	Pools       []wirePoolInfo
}

type wireResult struct {
	Chain  wireChainInfo
	Native wireFeedInfo
	Tokens []wireTokenPrices
}

type artifact struct {
	creation []byte
	in       abi.Arguments
	out      abi.Arguments
}

var (
	loadOnce sync.Once
	loaded   artifact
	loadErr  error
)

// load reads the compiled PriceLens once, taking the reply's shape from the
// IPriceLens carrier interface so the decoder cannot drift from the bytecode.
func load() (artifact, error) {
	loadOnce.Do(func() {
		lens, err := contracts.Load("PriceLens")
		if err != nil {
			loadErr = err
			return
		}
		lensABI, err := lens.Parsed()
		if err != nil {
			loadErr = err
			return
		}
		iface, err := contracts.Load("IPriceLens")
		if err != nil {
			loadErr = err
			return
		}
		ifaceABI, err := iface.Parsed()
		if err != nil {
			loadErr = err
			return
		}
		query, ok := ifaceABI.Methods["query"]
		if !ok {
			loadErr = errors.New("price: IPriceLens artifact has no query method")
			return
		}
		creation := lens.Creation()
		if len(creation) == 0 {
			loadErr = errors.New("price: PriceLens artifact has no creation code (run `make contracts`)")
			return
		}
		loaded = artifact{creation: creation, in: lensABI.Constructor.Inputs, out: query.Outputs}
	})
	return loaded, loadErr
}

// encode builds the eth_call payload: creation code with the request appended.
func encode(req wireRequest) ([]byte, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	args, err := a.in.Pack(req)
	if err != nil {
		return nil, fmt.Errorf("price: encode request: %w", err)
	}
	return append(append(make([]byte, 0, len(a.creation)+len(args)), a.creation...), args...), nil
}

// decode reads the constructor's returned "code" back into the reply struct.
func decode(data []byte) (wireResult, error) {
	a, err := load()
	if err != nil {
		return wireResult{}, err
	}
	if len(data) == 0 {
		return wireResult{}, errors.New("price: node returned no data (does this endpoint allow eth_call without a `to` address?)")
	}
	values, err := a.out.Unpack(data)
	if err != nil {
		return wireResult{}, fmt.Errorf("price: decode reply: %w", err)
	}
	var wrap struct{ Result wireResult }
	if err := a.out.Copy(&wrap, values); err != nil {
		return wireResult{}, fmt.Errorf("price: decode reply: %w", err)
	}
	return wrap.Result, nil
}
