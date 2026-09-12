// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {Ownable2Step} from "@openzeppelin/contracts/access/Ownable2Step.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {DNSNameLib} from "./DNSNameLib.sol";

/// @title HintSignedResolver
/// @notice An ENS resolver (ENSIP-10 wildcard, ERC-3668 CCIP-Read) for a chain the
///         HintRegistry is NOT on. It serves the same records as `HintResolver` under
///
///             <hex-address>.hints.<yourname>.eth
///             <hex-address>.<chainId>.hints.<yourname>.eth
///
///         but it cannot verify them against the registry's root, because the root
///         lives on another chain. Instead every gateway answer is signed by a key
///         this contract pins, and the callback checks that signature and an expiry.
///
/// What that buys, stated plainly: a reader learns that the publisher of this index
/// said so, recently. A gateway cannot forge an answer, but the publisher can, and
/// so can anyone holding its key. `HintResolver` on the registry's own chain is the
/// trust-minimized form; this one is the form ENS on mainnet can reach today.
///
/// Records answered here without a gateway: addr(node), addr(node, 60),
/// text "evmscan.registry" (the registry as eip155:<chain>:<address>), "evmscan.chain",
/// and "evmscan.signer" (the key answers are checked against). Every other text key —
/// evmscan.contracts, evmscan.epoch, evmscan.range, evmscan.root, evmscan.uri,
/// evmscan.hint — reverts OffchainLookup and comes back signed.
///
/// The signature is the one the ENS offchain-resolver pattern uses:
///   keccak256(0x1900 ‖ address(this) ‖ expires ‖ keccak256(request) ‖ keccak256(result))
/// where `request` is the full `resolve(name, data)` calldata the gateway was handed.
/// Binding the resolver's address is what keeps a signature from meaning anything
/// anywhere else.
contract HintSignedResolver is Ownable2Step {
    using DNSNameLib for bytes;

    /// @notice The key gateway answers must be signed with.
    address public signer;
    /// @notice The chain accounts are indexed on when the name names none.
    uint64 public immutable defaultChainId;
    /// @notice Where the registry that commits this index lives.
    uint64 public immutable registryChainId;
    address public immutable registry;

    string[] private _gateways;

    // ERC-165 / ENSIP-10 / resolver profile selectors.
    bytes4 private constant IFACE_ERC165 = 0x01ffc9a7;
    bytes4 private constant IFACE_EXTENDED_RESOLVER = 0x9061b923; // resolve(bytes,bytes)
    bytes4 private constant SEL_ADDR = 0x3b3b57de; // addr(bytes32)
    bytes4 private constant SEL_ADDR_COIN = 0xf1cb7e06; // addr(bytes32,uint256)
    bytes4 private constant SEL_TEXT = 0x59d1d43c; // text(bytes32,string)
    uint256 private constant COIN_TYPE_ETH = 60;

    bytes32 private constant KEY_REGISTRY = keccak256("evmscan.registry");
    bytes32 private constant KEY_CHAIN = keccak256("evmscan.chain");
    bytes32 private constant KEY_SIGNER = keccak256("evmscan.signer");

    event SignerChanged(address indexed signer);
    event GatewaysChanged(string[] urls);

    /// @dev ERC-3668.
    error OffchainLookup(address sender, string[] urls, bytes callData, bytes4 callbackFunction, bytes extraData);
    error UnsupportedResolverProfile(bytes4 selector);
    error InvalidAccountLabel();
    /// @dev The gateway's answer is older than it allowed itself to be.
    error Expired(uint64 expires);
    /// @dev The answer was signed by somebody other than `signer`.
    error BadSigner(address who);
    error BadSignerAddress();

    constructor(
        address signer_,
        string[] memory gateways_,
        uint64 defaultChainId_,
        uint64 registryChainId_,
        address registry_
    ) Ownable(msg.sender) {
        if (signer_ == address(0)) revert BadSignerAddress();
        signer = signer_;
        defaultChainId = defaultChainId_;
        registryChainId = registryChainId_;
        registry = registry_;
        _setGateways(gateways_);
        emit SignerChanged(signer_);
    }

    // --------------------------------------------------------------------
    // Owner
    // --------------------------------------------------------------------

    /// @notice Pin a new signing key: the publisher rotated, or the deployment moved.
    function setSigner(address signer_) external onlyOwner {
        if (signer_ == address(0)) revert BadSignerAddress();
        signer = signer_;
        emit SignerChanged(signer_);
    }

    /// @notice Replace the gateway list. Gateways are where to ask, never what to
    ///         believe: the signature decides that.
    function setGateways(string[] memory urls) external onlyOwner {
        _setGateways(urls);
    }

    function gateways() external view returns (string[] memory) {
        return _gateways;
    }

    function _setGateways(string[] memory urls) private {
        delete _gateways;
        for (uint256 i = 0; i < urls.length; i++) {
            _gateways.push(urls[i]);
        }
        emit GatewaysChanged(urls);
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
    /// @dev Everything a gateway answers goes through `resolveWithProof`; the whole
    ///      calldata is both the request the gateway decodes and the request the
    ///      signature covers, so nothing has to be re-encoded on either side.
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
            bytes32 k = keccak256(bytes(key));
            if (k == KEY_REGISTRY) {
                return abi.encode(
                    string(
                        abi.encodePacked(
                            "eip155:", DNSNameLib.toDecimalString(registryChainId), ":", DNSNameLib.toHexString(registry)
                        )
                    )
                );
            }
            if (k == KEY_CHAIN) return abi.encode(DNSNameLib.toDecimalString(chainId));
            if (k == KEY_SIGNER) return abi.encode(DNSNameLib.toHexString(signer));
            revert OffchainLookup(address(this), _gateways, msg.data, this.resolveWithProof.selector, msg.data);
        }
        revert UnsupportedResolverProfile(selector);
    }

    /// @notice ERC-3668 callback: check the gateway's signature and expiry, then hand
    ///         its result back as the profile's return value.
    /// @param response abi.encode(bytes result, uint64 expires, bytes signature)
    /// @param extraData the `resolve(name, data)` calldata the request was made with
    function resolveWithProof(bytes calldata response, bytes calldata extraData) external view returns (bytes memory) {
        (bytes memory result, uint64 expires, bytes memory sig) = abi.decode(response, (bytes, uint64, bytes));
        if (block.timestamp > expires) revert Expired(expires);
        bytes32 h = keccak256(abi.encodePacked(hex"1900", address(this), expires, keccak256(extraData), keccak256(result)));
        address who = ECDSA.recover(h, sig);
        if (who != signer) revert BadSigner(who);
        return result;
    }

    // --------------------------------------------------------------------
    // Name parsing, public so tests can table it
    // --------------------------------------------------------------------

    /// @notice Reads the account from the first label and the chain from the second
    ///         when it is decimal, else `defaultChainId`. Identical to HintResolver.
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
}
