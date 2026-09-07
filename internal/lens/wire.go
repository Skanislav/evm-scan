package lens

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
)

// The wire types below mirror the structs in contracts/src/AssetLens.sol.
//
// Two different rules bind them to that file, and both are load-bearing:
//
//   - Packing the request matches struct fields to ABI components *by name*, so a
//     field here must be ToCamelCase of the Solidity name (includeUri -> IncludeUri).
//   - Unpacking the reply copies the decoded tuple *by position*, so the field order
//     here must be the field order there, with no gaps and nothing extra.
//
// Drift in either direction is silent at compile time, which is why TestWireMatchesABI
// walks the compiled artifact and asserts both properties.

type wireTokenQuery struct {
	Token common.Address
	Ids   []*big.Int
}

type wireRequest struct {
	Account        common.Address
	Spenders       []common.Address
	Tokens         []wireTokenQuery
	IncludeUri     bool
	IncludeCode    bool
	EnumerateLimit *big.Int
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

type wireAccountInfo struct {
	Account     common.Address
	Balance     *big.Int
	CodeHash    common.Hash
	CodeSize    *big.Int
	IsContract  bool
	IsDelegated bool
	Delegate    common.Address
	Code        []byte
}

type wireTokenIdInfo struct {
	Id            *big.Int
	Owner         common.Address
	OwnerKnown    bool
	Balance       *big.Int
	BalanceKnown  bool
	Approved      common.Address
	ApprovedKnown bool
	Uri           string
	UriTruncated  bool
}

type wireTokenInfo struct {
	Token          common.Address
	IsContract     bool
	Standard       uint8
	SupportsErc165 bool
	IsErc721       bool
	IsErc1155      bool
	IsEnumerable   bool
	Symbol         string
	Name           string
	Decimals       uint8
	HasDecimals    bool
	TotalSupply    *big.Int
	HasTotalSupply bool
	Balance        *big.Int
	HasBalance     bool
	Allowances     []*big.Int
	AllowanceKnown []bool
	ApprovedForAll []bool
	Ids            []wireTokenIdInfo
}

type wireResult struct {
	Chain   wireChainInfo
	Account wireAccountInfo
	Tokens  []wireTokenInfo
}

// artifact holds the compiled lens: its creation code, the constructor's argument
// encoding, and the reply's decoding.
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

// load reads both compiled artifacts once.
//
// The reply's type comes from IAssetLens.query rather than from a hand-written
// decoder: the lens returns exactly what that signature would return, so taking the
// shape from the same compiler output that produced the bytecode removes the class
// of bug where a struct is reordered in Solidity and the Go decoder keeps happily
// reading the old layout.
func load() (artifact, error) {
	loadOnce.Do(func() {
		lens, err := contracts.Load("AssetLens")
		if err != nil {
			loadErr = err
			return
		}
		lensABI, err := lens.Parsed()
		if err != nil {
			loadErr = err
			return
		}
		iface, err := contracts.Load("IAssetLens")
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
			loadErr = errors.New("lens: IAssetLens artifact has no query method")
			return
		}
		creation := lens.Creation()
		if len(creation) == 0 {
			loadErr = errors.New("lens: AssetLens artifact has no creation code (run `make contracts`)")
			return
		}
		loaded = artifact{creation: creation, in: lensABI.Constructor.Inputs, out: query.Outputs}
	})
	return loaded, loadErr
}

// encode builds the eth_call payload: creation code with the request appended, which
// is how constructor arguments reach a contract that is never deployed.
func encode(req wireRequest) ([]byte, error) {
	a, err := load()
	if err != nil {
		return nil, err
	}
	args, err := a.in.Pack(req)
	if err != nil {
		return nil, fmt.Errorf("lens: encode request: %w", err)
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
		return wireResult{}, errors.New("lens: node returned no data (does this endpoint allow eth_call without a `to` address?)")
	}
	values, err := a.out.Unpack(data)
	if err != nil {
		return wireResult{}, fmt.Errorf("lens: decode reply: %w", err)
	}
	// A single tuple return copies into the *first field* of the destination, hence
	// the wrapper rather than unpacking straight into a wireResult.
	var wrap struct{ Result wireResult }
	if err := a.out.Copy(&wrap, values); err != nil {
		return wireResult{}, fmt.Errorf("lens: decode reply: %w", err)
	}
	return wrap.Result, nil
}
