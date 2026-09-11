// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {HintRegistry} from "./HintRegistry.sol";
import {DNSNameLib} from "./DNSNameLib.sol";

/// @title HintResolver
/// @notice An ENS resolver (ENSIP-10 wildcard, ERC-3668 CCIP-Read) that serves the
///         committed `account -> contracts` index as ENS records.
///
/// Bind it as the resolver of a label such as `hints.<yourname>.eth` in an ENSv2
/// registry and every account gets a name without anyone registering one:
///
///     <hex-address>.hints.<yourname>.eth
///     <hex-address>.<chainId>.hints.<yourname>.eth      (second label overrides the default chain)
///
/// The Universal Resolver finds this contract for any such name (it is the deepest
/// resolver on the path), sees it supports IExtendedResolver, and calls
/// `resolve(name, data)`. The account is parsed from the first label; the `node`
/// inside `data` is ignored, as ENSIP-10 allows, because identity comes from the name.
///
/// Records:
///
///   addr(node)                     the account itself
///   addr(node, 60)                 the account, as 20 bytes
///   text(node, "evmscan.contracts") the contracts the account touched, verified. Reverts
///                                  OffchainLookup at the registry's gateways; the callback
///                                  hands the answer to HintRegistry.contractsOfCallback,
///                                  which verifies it against the latest finalized root.
///   text(node, "evmscan.epoch")    id of that epoch
///   text(node, "evmscan.range")    "<fromBlock>-<toBlock>" the epoch covers
///   text(node, "evmscan.root")     the epoch's index root
///   text(node, "evmscan.uri")      the pointer that epoch committed: the index table, and
///                                  the digest of the membership filter beside it
///   text(node, "evmscan.registry") this resolver's HintRegistry
///   text(node, "evmscan.chain")    the chain the account was indexed on
///
/// Nothing here is configured by hand: gateways, epochs and roots are read from the
/// registry at call time. A gateway can withhold an answer but cannot forge one, and
/// cannot serve a stale epoch, exactly as with `HintRegistry.contractsOf`.
contract HintResolver {
    using DNSNameLib for bytes;

    HintRegistry public immutable registry;
    uint64 public immutable defaultChainId;

    // ERC-165 / ENSIP-10 / resolver profile selectors.
    bytes4 private constant IFACE_ERC165 = 0x01ffc9a7;
    bytes4 private constant IFACE_EXTENDED_RESOLVER = 0x9061b923; // resolve(bytes,bytes)
    bytes4 private constant SEL_ADDR = 0x3b3b57de; // addr(bytes32)
    bytes4 private constant SEL_ADDR_COIN = 0xf1cb7e06; // addr(bytes32,uint256)
    bytes4 private constant SEL_TEXT = 0x59d1d43c; // text(bytes32,string)
    uint256 private constant COIN_TYPE_ETH = 60;

    bytes32 private constant KEY_CONTRACTS = keccak256("evmscan.contracts");
    bytes32 private constant KEY_EPOCH = keccak256("evmscan.epoch");
    bytes32 private constant KEY_RANGE = keccak256("evmscan.range");
    bytes32 private constant KEY_ROOT = keccak256("evmscan.root");
    bytes32 private constant KEY_URI = keccak256("evmscan.uri");
    bytes32 private constant KEY_REGISTRY = keccak256("evmscan.registry");
    bytes32 private constant KEY_CHAIN = keccak256("evmscan.chain");

    /// @dev ERC-3668.
    error OffchainLookup(address sender, string[] urls, bytes callData, bytes4 callbackFunction, bytes extraData);
    /// @dev The profile `data` asks for is not one this resolver serves. Same shape the
    ///      Universal Resolver reports for an unknown profile.
    error UnsupportedResolverProfile(bytes4 selector);
    /// @dev The first label is not a hex account address.
    error InvalidAccountLabel();
    /// @dev The registry holds no finalized commitment for this chain yet.
    error NoFinalizedEpoch(uint64 chainId);

    constructor(HintRegistry registry_, uint64 defaultChainId_) {
        registry = registry_;
        defaultChainId = defaultChainId_;
    }

    // --------------------------------------------------------------------
    // ERC-165
    // --------------------------------------------------------------------

    function supportsInterface(bytes4 id) external pure returns (bool) {
        return id == IFACE_ERC165 || id == IFACE_EXTENDED_RESOLVER;
    }

    // --------------------------------------------------------------------
    // ENSIP-10
    // --------------------------------------------------------------------

    /// @notice Resolve a profile for a DNS-encoded name under this resolver.
    /// @dev Reverts `OffchainLookup` only for `text(node, "evmscan.contracts")`; every
    ///      other record is answered from chain state in this call.
    function resolve(bytes calldata name, bytes calldata data) external view returns (bytes memory) {
        (address account, uint64 chainId, bool ok) = parseName(name);
        if (!ok) revert InvalidAccountLabel();
        if (data.length < 4) revert UnsupportedResolverProfile(bytes4(0));
        bytes4 selector = bytes4(data[:4]);

        if (selector == SEL_ADDR) {
            return abi.encode(account);
        }
        if (selector == SEL_ADDR_COIN) {
            (, uint256 coinType) = abi.decode(data[4:], (bytes32, uint256));
            if (coinType == COIN_TYPE_ETH) return abi.encode(abi.encodePacked(account));
            return abi.encode(bytes(""));
        }
        if (selector == SEL_TEXT) {
            (, string memory key) = abi.decode(data[4:], (bytes32, string));
            return abi.encode(_text(account, chainId, key));
        }
        revert UnsupportedResolverProfile(selector);
    }

    /// @notice ERC-3668 callback for `text(node, "evmscan.contracts")`.
    /// @param response abi.encode(uint256 epochId, address[] assets, bytes32[] proof), as
    ///        the registry's gateway already produces for `contractsOf`.
    /// @param extraData abi.encode(uint64 chainId, address account), the shape the
    ///        registry's own callback expects, so verification is delegated wholesale:
    ///        StaleEpoch, BadProof and Unsorted bubble up unchanged.
    function resolveCallback(bytes calldata response, bytes calldata extraData) external view returns (bytes memory) {
        address[] memory assets = registry.contractsOfCallback(response, extraData);
        return abi.encode(contractsText(assets));
    }

    // --------------------------------------------------------------------
    // Name parsing and record formats, public so tests can table them
    // --------------------------------------------------------------------

    /// @notice Reads the account from the first label and the chain from the second
    ///         when it is decimal, else `defaultChainId`.
    function parseName(bytes calldata name) public view returns (address account, uint64 chainId, bool ok) {
        bytes memory n = name;
        (bytes memory first, uint256 next) = n.readLabel(0);
        (account, ok) = first.parseHexAddress();
        if (!ok) return (address(0), 0, false);
        chainId = defaultChainId;
        (bytes memory second,) = n.readLabel(next);
        (uint64 explicitChain, bool isChain) = second.parseDecimal();
        if (isChain) chainId = explicitChain;
        return (account, chainId, true);
    }

    /// @notice The `evmscan.contracts` record format: lowercase hex addresses joined by commas.
    function contractsText(address[] memory assets) public pure returns (string memory) {
        bytes memory out;
        for (uint256 i = 0; i < assets.length; i++) {
            if (i > 0) out = abi.encodePacked(out, ",");
            out = abi.encodePacked(out, DNSNameLib.toHexString(assets[i]));
        }
        return string(out);
    }

    // --------------------------------------------------------------------
    // Internals
    // --------------------------------------------------------------------

    function _text(address account, uint64 chainId, string memory key) private view returns (string memory) {
        bytes32 k = keccak256(bytes(key));
        if (k == KEY_REGISTRY) return DNSNameLib.toHexString(address(registry));
        if (k == KEY_CHAIN) return DNSNameLib.toDecimalString(chainId);

        if (k == KEY_CONTRACTS) {
            (bool found,,) = registry.latestFinalizedEpoch(chainId);
            if (!found) revert NoFinalizedEpoch(chainId);
            revert OffchainLookup(
                address(this),
                registry.gateways(),
                abi.encodeWithSelector(HintRegistry.contractsOf.selector, chainId, account),
                this.resolveCallback.selector,
                abi.encode(chainId, account)
            );
        }
        if (k == KEY_EPOCH || k == KEY_RANGE || k == KEY_ROOT || k == KEY_URI) {
            (bool found, uint256 epochId, HintRegistry.Epoch memory e) = registry.latestFinalizedEpoch(chainId);
            if (!found) return "";
            if (k == KEY_EPOCH) return DNSNameLib.toDecimalString(epochId);
            if (k == KEY_ROOT) return DNSNameLib.toHexString(e.root);
            // Returned verbatim. The publisher named this URI inside the same bonded
            // transaction as the root, and what hangs off it — the index table, and
            // the membership filter's digest in the manifest beside it — is the
            // caller's to fetch and parse. A resolver that parsed URIs would be
            // committing this contract to a document format it cannot verify.
            if (k == KEY_URI) return e.uri;
            return string(
                abi.encodePacked(
                    DNSNameLib.toDecimalString(e.fromBlock), "-", DNSNameLib.toDecimalString(e.toBlock)
                )
            );
        }
        return "";
    }
}
