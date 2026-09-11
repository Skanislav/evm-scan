package ens

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/ccip"
)

// The ENSv2 surface evm-scan touches, as minimal ABIs. These are the functions a
// name owner needs to hang a resolver under a label, and nothing else; the
// contracts-v2 repository is the reference. The beta is explicitly unfinished, so
// every address comes in as a flag and every call is preceded by a probe. Only
// cmd/evmscan-ens and cmd/evmscan-verify use any of this; the daemon does not.

const registryABIJSON = `[
 {"type":"function","name":"getSubregistry","stateMutability":"view","inputs":[{"name":"label","type":"string"}],"outputs":[{"name":"","type":"address"}]},
 {"type":"function","name":"getResolver","stateMutability":"view","inputs":[{"name":"label","type":"string"}],"outputs":[{"name":"","type":"address"}]},
 {"type":"function","name":"getOwner","stateMutability":"view","inputs":[{"name":"anyId","type":"uint256"}],"outputs":[{"name":"","type":"address"}]},
 {"type":"function","name":"getExpiry","stateMutability":"view","inputs":[{"name":"anyId","type":"uint256"}],"outputs":[{"name":"","type":"uint64"}]},
 {"type":"function","name":"register","stateMutability":"nonpayable","inputs":[
   {"name":"label","type":"string"},{"name":"owner","type":"address"},{"name":"registry","type":"address"},
   {"name":"resolver","type":"address"},{"name":"roleBitmap","type":"uint256"},{"name":"expiry","type":"uint64"}],
   "outputs":[{"name":"tokenId","type":"uint256"}]},
 {"type":"function","name":"setSubregistry","stateMutability":"nonpayable","inputs":[{"name":"anyId","type":"uint256"},{"name":"registry","type":"address"}],"outputs":[]},
 {"type":"function","name":"setResolver","stateMutability":"nonpayable","inputs":[{"name":"anyId","type":"uint256"},{"name":"resolver","type":"address"}],"outputs":[]},
 {"type":"function","name":"supportsInterface","stateMutability":"view","inputs":[{"name":"id","type":"bytes4"}],"outputs":[{"name":"","type":"bool"}]}
]`

const factoryABIJSON = `[
 {"type":"function","name":"deployProxy","stateMutability":"nonpayable","inputs":[
   {"name":"implementation","type":"address"},{"name":"salt","type":"uint256"},{"name":"data","type":"bytes"}],
   "outputs":[{"name":"","type":"address"}]},
 {"type":"function","name":"verifyContract","stateMutability":"view","inputs":[{"name":"proxy","type":"address"}],"outputs":[{"name":"implementation","type":"address"}]},
 {"type":"event","name":"ProxyDeployed","anonymous":false,"inputs":[
   {"name":"sender","type":"address","indexed":true},{"name":"proxyAddress","type":"address","indexed":true},
   {"name":"salt","type":"uint256","indexed":false},{"name":"implementation","type":"address","indexed":false}]}
]`

const userRegistryInitABIJSON = `[
 {"type":"function","name":"initialize","stateMutability":"nonpayable","inputs":[{"name":"rootAccount","type":"address"},{"name":"roleBitmap","type":"uint256"}],"outputs":[]}
]`

const universalResolverABIJSON = `[
 {"type":"function","name":"findResolver","stateMutability":"view","inputs":[{"name":"name","type":"bytes"}],"outputs":[{"name":"resolver","type":"address"},{"name":"node","type":"bytes32"},{"name":"offset","type":"uint256"}]},
 {"type":"function","name":"resolve","stateMutability":"view","inputs":[{"name":"name","type":"bytes"},{"name":"data","type":"bytes"}],"outputs":[{"name":"result","type":"bytes"},{"name":"resolver","type":"address"}]},
 {"type":"function","name":"reverse","stateMutability":"view","inputs":[{"name":"lookupAddress","type":"bytes"},{"name":"coinType","type":"uint256"}],"outputs":[{"name":"primary","type":"string"},{"name":"resolver","type":"address"},{"name":"reverseResolver","type":"address"}]},
 {"type":"function","name":"supportsInterface","stateMutability":"view","inputs":[{"name":"id","type":"bytes4"}],"outputs":[{"name":"","type":"bool"}]},
 {"type":"function","name":"ROOT_REGISTRY","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
 {"type":"error","name":"ResolverNotFound","inputs":[{"name":"name","type":"bytes"}]},
 {"type":"error","name":"ResolverNotContract","inputs":[{"name":"name","type":"bytes"},{"name":"resolver","type":"address"}]},
 {"type":"error","name":"UnsupportedResolverProfile","inputs":[{"name":"selector","type":"bytes4"}]},
 {"type":"error","name":"ResolverError","inputs":[{"name":"errorData","type":"bytes"}]},
 {"type":"error","name":"ReverseAddressMismatch","inputs":[{"name":"primary","type":"string"},{"name":"primaryAddress","type":"bytes"}]},
 {"type":"error","name":"HttpError","inputs":[{"name":"status","type":"uint16"},{"name":"message","type":"string"}]}
]`

var (
	// RegistryABI is IStandardRegistry's owner-facing subset (PermissionedRegistry,
	// ETHRegistry, UserRegistry all implement it).
	RegistryABI = mustABI(registryABIJSON)
	// FactoryABI is ENS's VerifiableFactory.
	FactoryABI = mustABI(factoryABIJSON)
	// UserRegistryInitABI is the initializer a VerifiableFactory proxy runs.
	UserRegistryInitABI = mustABI(userRegistryInitABIJSON)
	// UniversalResolverABI is the shared v1/v2 surface plus the errors it raises.
	UniversalResolverABI = mustABI(universalResolverABIJSON)
)

func mustABI(js string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(js))
	if err != nil {
		panic(err)
	}
	return parsed
}

// PermissionedRegistry roles (RegistryRolesLib). A role's admin bit is the role
// shifted by 128.
const (
	RoleRegistrar      = uint64(1) << 0
	RoleUnregister     = uint64(1) << 12
	RoleRenew          = uint64(1) << 16
	RoleSetSubregistry = uint64(1) << 20
	RoleSetResolver    = uint64(1) << 24
)

// WithAdmin returns roles together with their admin bits.
func WithAdmin(roles uint64) *big.Int {
	r := new(big.Int).SetUint64(roles)
	admin := new(big.Int).Lsh(new(big.Int).SetUint64(roles), 128)
	return r.Or(r, admin)
}

// OwnerRoles is what a user registry's root account gets: it can register labels,
// renew and remove them, and point them at resolvers and subregistries.
func OwnerRoles() *big.Int {
	return WithAdmin(RoleRegistrar | RoleUnregister | RoleRenew | RoleSetSubregistry | RoleSetResolver)
}

// LabelRoles is what the owner of one label gets: pointing it at a resolver or a
// subregistry.
func LabelRoles() *big.Int {
	return WithAdmin(RoleSetSubregistry | RoleSetResolver)
}

// LabelID is the uint256 a PermissionedRegistry addresses a label by:
// uint256(keccak256(label)). The registry masks in a version in the low 32 bits
// itself, so any id with the right upper bits works as `anyId`.
func LabelID(label string) *big.Int {
	return new(big.Int).SetBytes(crypto.Keccak256([]byte(label)))
}

// UserRegistrySalt is the CREATE2 salt ens-cli uses for a name's user registry:
// keccak256(abi.encode(keccak256("UserRegistry"), namehash(name), 0)). Using the
// same salt means a registry deployed by either tool lands at the same address.
func UserRegistrySalt(name string) (*big.Int, error) {
	norm, err := Normalize(name)
	if err != nil {
		return nil, err
	}
	node := Namehash(norm)
	args := mustArgs("bytes32", "bytes32", "uint256")
	packed, err := args.Pack([32]byte(crypto.Keccak256([]byte("UserRegistry"))), [32]byte(node), big.NewInt(0))
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(crypto.Keccak256(packed)), nil
}

// FindResolver asks a Universal Resolver which resolver serves a name, and at which
// label offset it was found (0 = the name itself; larger = a parent, i.e. a wildcard).
func FindResolver(ctx context.Context, call ccip.Caller, ur common.Address, name string) (resolver common.Address, node common.Hash, offset uint64, err error) {
	norm, err := Normalize(name)
	if err != nil {
		return common.Address{}, common.Hash{}, 0, err
	}
	data, err := UniversalResolverABI.Pack("findResolver", DNSEncode(norm))
	if err != nil {
		return common.Address{}, common.Hash{}, 0, err
	}
	out, err := call(ctx, ur, data)
	if err != nil {
		if rd, ok := ccip.RevertData(err); ok {
			return common.Address{}, common.Hash{}, 0, classifyRevert(rd)
		}
		return common.Address{}, common.Hash{}, 0, err
	}
	vals, err := UniversalResolverABI.Unpack("findResolver", out)
	if err != nil {
		return common.Address{}, common.Hash{}, 0, err
	}
	resolver, _ = vals[0].(common.Address)
	n, _ := vals[1].([32]byte)
	off, _ := vals[2].(*big.Int)
	if off != nil {
		offset = off.Uint64()
	}
	return resolver, common.Hash(n), offset, nil
}

// ResolveText asks an ENSIP-10 resolver directly for text(node, key), following an
// ERC-3668 OffchainLookup if it raises one. resolverABI must include resolve, the
// callback the resolver names, and the OffchainLookup error; HintResolver's
// artifact ABI does. urls override the resolver's gateway list when non-empty.
//
// This is a verification tool's path, not the daemon's: it follows a gateway on
// purpose, to check what the gateway serves against the chain.
func ResolveText(ctx context.Context, call ccip.Caller, resolverABI abi.ABI, resolver common.Address, name, key string, hc *http.Client, urls []string) (string, error) {
	norm, err := Normalize(name)
	if err != nil {
		return "", err
	}
	profile, err := TextCallData(Namehash(norm), key)
	if err != nil {
		return "", err
	}
	data, err := resolverABI.Pack("resolve", DNSEncode(norm), profile)
	if err != nil {
		return "", fmt.Errorf("ens: pack resolve: %w", err)
	}
	out, err := ccip.Resolve(ctx, call, resolverABI, resolver, data, hc, urls)
	if err != nil {
		return "", err
	}
	// Whether the answer came straight back or through the callback, it is one
	// ABI-encoded bytes value holding an ABI-encoded string.
	inner, err := DecodeBytes(out)
	if err != nil {
		return "", fmt.Errorf("ens: decode resolver result: %w", err)
	}
	return DecodeString(inner)
}
