// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {Nonces} from "@openzeppelin/contracts/utils/Nonces.sol";

import {HintRegistry} from "./HintRegistry.sol";
import {HintResolver} from "./HintResolver.sol";
import {DNSNameLib} from "./DNSNameLib.sol";

/// @title HintAliasResolver
/// @notice HintResolver plus human-readable names: `alice.hints.<yourname>.eth`
///         beside `<hex-address>.hints.<yourname>.eth`.
///
/// The base resolver gives every account a name without anyone registering one, by
/// reading the account out of the first label as hex. That is the right default and
/// it is unreadable. This subclass keeps it and adds one mapping: a label an account
/// has claimed resolves to that account, and everything downstream — addr, the
/// records, the ERC-3668 round trip to the registry's gateway, the verification
/// against the latest finalized root — is the base class, unchanged. A claimed name
/// is therefore exactly as trustworthy as a hex one, because the answer is still
/// proven against the root and the name only chooses whose answer it is.
///
/// **A claim is signed, not sold, and not adjudicated.** The signature proves the
/// claimant controls the address; it proves nothing whatsoever about the label. Names
/// are first-come-first-served, so `alice` belongs to whoever asked first, and this
/// contract has no opinion about whether they are Alice. Anything that needs to know
/// *who* an address is must read `addr` and check it against something else.
///
/// **Claiming is free to the claimant.** `claimFor` takes a signature and can be sent
/// by anyone, so an operator can pay the gas for a reader who holds none — which is
/// the only reason this is a signed message rather than a transaction. The carrier is
/// not recorded and gets nothing: the name belongs to the signer either way.
contract HintAliasResolver is HintResolver, EIP712, Nonces {
    using DNSNameLib for bytes;

    /// @dev Labels shorter than this cannot be claimed. Three is not a security
    ///      boundary, it just keeps the scarcest names out of a demo's way.
    uint256 public constant MIN_LABEL_LENGTH = 3;
    /// @dev One DNS label's ceiling.
    uint256 public constant MAX_LABEL_LENGTH = 63;

    bytes32 private constant CLAIM_TYPEHASH =
        keccak256("Claim(string label,address account,uint256 nonce,uint256 deadline)");
    bytes32 private constant RELEASE_TYPEHASH =
        keccak256("Release(string label,address account,uint256 nonce,uint256 deadline)");

    /// @notice The account a claimed label resolves to, keyed by `keccak256(label)`.
    mapping(bytes32 => address) public accountOfLabel;

    /// @notice The label an account holds, or "" — the reverse of `accountOfLabel`.
    /// @dev An account holds at most one name, and claiming a second releases the
    ///      first in the same transaction. That is a real restriction and it is the
    ///      point: without it this mapping could only name one of several labels and
    ///      would disagree with the forward one, and a reader asking "what is this
    ///      account called" would get an answer that depends on claim order. One name
    ///      per account is also what a client wants to show.
    mapping(address => string) public labelOfAccount;

    event NameClaimed(bytes32 indexed labelHash, string label, address indexed account);
    event NameReleased(bytes32 indexed labelHash, string label, address indexed account);

    /// @dev The signature is past its deadline.
    error ExpiredSignature();
    /// @dev The recovered signer is not the account the claim names.
    error BadSigner();
    /// @dev The label is empty, too long, too short, or holds a character this
    ///      contract will not store.
    error BadLabel();
    /// @dev A label that parses as a hex address may not be claimed: it would shadow
    ///      the account whose address it spells.
    error LabelIsAddress();
    /// @dev The label is already claimed by a different account.
    error LabelTaken(address account);
    /// @dev Releasing a label the signer does not hold.
    error NotLabelOwner(address account);

    constructor(HintRegistry registry_, uint64 defaultChainId_)
        HintResolver(registry_, defaultChainId_)
        EIP712("evm-scan hint name", "1")
    {}

    // --------------------------------------------------------------------
    // Claiming
    // --------------------------------------------------------------------

    /// @notice Point `label` at `account`, authorised by `account`'s EIP-712 signature
    ///         and paid for by whoever sends the transaction.
    /// @dev `Claim(string label,address account,uint256 nonce,uint256 deadline)` with
    ///      this contract as the verifying contract and this chain in the domain, so a
    ///      signature cannot be replayed onto another deployment or another chain. The
    ///      nonce is `nonces(account)` and is spent whether or not the mapping changed,
    ///      so one signature is good for exactly one transaction — the same rule
    ///      `HintRegistry.voteFor` uses.
    ///
    ///      Re-claiming a label the signer already holds is allowed and costs a nonce:
    ///      it is how a claim is refreshed, and it is not a way to take one.
    function claimFor(
        string calldata label,
        address account,
        uint256 deadline,
        bytes calldata signature
    ) external {
        if (account == address(0)) revert BadSigner();
        bytes32 labelHash = _requireClaimableLabel(label);

        address held = accountOfLabel[labelHash];
        if (held != address(0) && held != account) revert LabelTaken(held);

        _authorize(CLAIM_TYPEHASH, label, account, deadline, signature);

        // An account holds one name. Claiming a second frees the first here rather
        // than leaving it pointing at an account that no longer answers to it.
        string memory previous = labelOfAccount[account];
        bytes32 previousHash = keccak256(bytes(previous));
        if (bytes(previous).length != 0 && previousHash != labelHash) {
            delete accountOfLabel[previousHash];
            emit NameReleased(previousHash, previous, account);
        }

        accountOfLabel[labelHash] = account;
        labelOfAccount[account] = label;
        emit NameClaimed(labelHash, label, account);
    }

    /// @notice Give up a claimed label, freeing it for anyone else.
    /// @dev Signed by the holder, carried by anyone, for the same reason as `claimFor`:
    ///      a reader who cannot pay gas must still be able to undo what they published.
    function releaseFor(
        string calldata label,
        address account,
        uint256 deadline,
        bytes calldata signature
    ) external {
        bytes32 labelHash = keccak256(bytes(label));
        address held = accountOfLabel[labelHash];
        if (held == address(0) || held != account) revert NotLabelOwner(held);

        _authorize(RELEASE_TYPEHASH, label, account, deadline, signature);

        delete accountOfLabel[labelHash];
        delete labelOfAccount[account];
        emit NameReleased(labelHash, label, account);
    }

    // --------------------------------------------------------------------
    // Resolution
    // --------------------------------------------------------------------

    /// @notice As `HintResolver.parseName`, but a first label that is not hex is
    ///         looked up as a claimed name before the name is rejected.
    /// @dev Hex wins. A claimed label can never be hex (`_requireClaimableLabel`
    ///      refuses it), so the two namespaces cannot collide and this order is a
    ///      statement of that rather than a tie-break.
    function parseName(bytes calldata name)
        public
        view
        override
        returns (address account, uint64 chainId, bool ok)
    {
        (account, chainId, ok) = super.parseName(name);
        if (ok) return (account, chainId, true);

        bytes memory n = name;
        (bytes memory first, uint256 next) = n.readLabel(0);
        account = accountOfLabel[keccak256(first)];
        if (account == address(0)) return (address(0), 0, false);

        chainId = defaultChainId;
        (bytes memory second,) = n.readLabel(next);
        (uint64 explicitChain, bool isChain) = second.parseDecimal();
        if (isChain) chainId = explicitChain;
        return (account, chainId, true);
    }

    // --------------------------------------------------------------------
    // Internals
    // --------------------------------------------------------------------

    /// @dev Recovers the signer and spends the nonce. Shared so a claim and a release
    ///      cannot differ by accident in how they are authorised.
    function _authorize(
        bytes32 typeHash,
        string calldata label,
        address account,
        uint256 deadline,
        bytes calldata signature
    ) private {
        if (block.timestamp > deadline) revert ExpiredSignature();
        uint256 nonce = _useNonce(account);
        bytes32 structHash = keccak256(abi.encode(typeHash, keccak256(bytes(label)), account, nonce, deadline));
        if (ECDSA.recover(_hashTypedDataV4(structHash), signature) != account) revert BadSigner();
    }

    /// @dev Only lowercase ASCII letters, digits and hyphens, so that the label stored
    ///      here and the label a client normalises (NFC + lowercase) are the same
    ///      bytes. This contract cannot run ENSIP-15, and a label it cannot normalise
    ///      is a label that could resolve differently in two clients; refusing those
    ///      outright is cheaper and more honest than storing one and hoping.
    ///      A hyphen may not lead or trail, which is also what keeps `-` alone out.
    function _requireClaimableLabel(string calldata label) private pure returns (bytes32) {
        bytes memory b = bytes(label);
        if (b.length < MIN_LABEL_LENGTH || b.length > MAX_LABEL_LENGTH) revert BadLabel();
        if (b[0] == "-" || b[b.length - 1] == "-") revert BadLabel();
        for (uint256 i = 0; i < b.length; i++) {
            bytes1 c = b[i];
            bool okChar = (c >= "a" && c <= "z") || (c >= "0" && c <= "9") || c == "-";
            if (!okChar) revert BadLabel();
        }
        // A label that spells an address must stay the address's own, or a claimant
        // could occupy somebody else's hex name and answer for them.
        (, bool isAddress) = b.parseHexAddress();
        if (isAddress) revert LabelIsAddress();
        return keccak256(b);
    }
}
