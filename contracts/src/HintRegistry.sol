// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title HintRegistry
/// @notice Permissionless, optimistic registry of *discovery hints* for EVM asset indexing.
///
/// The registry holds two kinds of public commitment:
///
///  1. **Asset hints** — anyone may register a `(chainId, token)` pair. Registration is
///     permissionless and unvalidated: it is a request that indexers scan that contract's
///     full log history at least once. Nothing here asserts the contract is legitimate.
///
///  2. **Index commitments** — an indexer publishes a merkle root over the
///     `account -> assets touched` table it derived for a block range, plus a URI to the
///     full table. The root is *optimistic*: it finalizes after a challenge window unless
///     someone bonds a challenge against it.
///
/// Nothing here is a source of truth. The underlying logs are public on the source chain;
/// this contract only makes it cheap to discover *which* contracts are worth pulling
/// history for. A consumer that needs certainty re-derives the data from the chain.
contract HintRegistry {
    // --------------------------------------------------------------------
    // Types
    // --------------------------------------------------------------------

    /// @dev Sentinels, not EIP numbers: 721 and 1155 do not fit in a uint8.
    uint8 public constant KIND_UNKNOWN = 0;
    uint8 public constant KIND_ERC20 = 20;
    uint8 public constant KIND_ERC721 = 21;
    uint8 public constant KIND_ERC1155 = 55;

    enum EpochStatus {
        None,
        Proposed,
        Challenged,
        Finalized,
        Rejected
    }

    struct Asset {
        uint64 chainId;
        address token;
        uint8 kind;
        /// @dev Hint for where the indexer should start scanning (usually the deploy
        ///      block). The indexer may correct this downward; it is not trusted.
        uint64 fromBlock;
        address registrant;
        uint64 registeredAt;
        uint256 bond;
        bool active;
    }

    struct Epoch {
        uint64 chainId;
        uint64 fromBlock;
        uint64 toBlock;
        /// @dev Merkle root over leaves `keccak256(abi.encode(account, chainId, assetsHash))`.
        bytes32 root;
        /// @dev Pointer to the full index table (ipfs://, https://, ...).
        string uri;
        address publisher;
        address challenger;
        uint256 bond;
        uint64 publishedAt;
        uint64 challengeDeadline;
        EpochStatus status;
    }

    // --------------------------------------------------------------------
    // Storage
    // --------------------------------------------------------------------

    address public arbiter;
    uint256 public assetBond;
    uint256 public publisherBond;
    uint256 public challengeWindow;

    mapping(bytes32 => Asset) private _assets;
    bytes32[] private _assetKeys;

    Epoch[] private _epochs;
    /// @notice chainId => id of the most recent finalized epoch, +1 (0 means none).
    mapping(uint64 => uint256) private _latestFinalized;

    // --------------------------------------------------------------------
    // Events
    // --------------------------------------------------------------------

    event AssetRegistered(
        bytes32 indexed key,
        uint64 indexed chainId,
        address indexed token,
        uint8 kind,
        uint64 fromBlock,
        address registrant
    );
    event AssetRevoked(bytes32 indexed key, address indexed registrant);
    event IndexPublished(
        uint256 indexed epochId,
        uint64 indexed chainId,
        uint64 fromBlock,
        uint64 toBlock,
        bytes32 root,
        string uri,
        address publisher
    );
    event IndexChallenged(uint256 indexed epochId, address indexed challenger);
    event IndexResolved(uint256 indexed epochId, EpochStatus status);
    event ArbiterUpdated(address indexed previous, address indexed next);

    // --------------------------------------------------------------------
    // Errors
    // --------------------------------------------------------------------

    error NotArbiter();
    error NotRegistrant();
    error AlreadyRegistered();
    error UnknownAsset();
    error UnknownEpoch();
    error BadBond();
    error BadRange();
    error BadStatus();
    error WindowOpen();
    error WindowClosed();
    error TransferFailed();

    modifier onlyArbiter() {
        if (msg.sender != arbiter) revert NotArbiter();
        _;
    }

    constructor(address arbiter_, uint256 assetBond_, uint256 publisherBond_, uint256 challengeWindow_) {
        arbiter = arbiter_;
        assetBond = assetBond_;
        publisherBond = publisherBond_;
        challengeWindow = challengeWindow_;
        emit ArbiterUpdated(address(0), arbiter_);
    }

    // --------------------------------------------------------------------
    // Asset hints
    // --------------------------------------------------------------------

    /// @notice Deterministic key for an asset hint.
    function assetKey(uint64 chainId, address token) public pure returns (bytes32) {
        return keccak256(abi.encodePacked(chainId, token));
    }

    /// @notice Register a `(chainId, token)` pair for indexing. Permissionless.
    /// @dev The bond exists to price spam, not to confer legitimacy. It is refundable
    ///      via `revokeAsset`.
    function registerAsset(uint64 chainId, address token, uint8 kind, uint64 fromBlock)
        external
        payable
        returns (bytes32 key)
    {
        if (msg.value != assetBond) revert BadBond();
        key = assetKey(chainId, token);
        if (_assets[key].active) revert AlreadyRegistered();

        _assets[key] = Asset({
            chainId: chainId,
            token: token,
            kind: kind,
            fromBlock: fromBlock,
            registrant: msg.sender,
            registeredAt: uint64(block.timestamp),
            bond: msg.value,
            active: true
        });
        _assetKeys.push(key);

        emit AssetRegistered(key, chainId, token, kind, fromBlock, msg.sender);
    }

    /// @notice Withdraw an asset hint and reclaim its bond.
    /// @dev Indexers keep whatever they already derived; this only stops future scanning.
    function revokeAsset(bytes32 key) external {
        Asset storage a = _assets[key];
        if (!a.active) revert UnknownAsset();
        if (a.registrant != msg.sender) revert NotRegistrant();

        uint256 refund = a.bond;
        a.active = false;
        a.bond = 0;

        emit AssetRevoked(key, msg.sender);
        _pay(msg.sender, refund);
    }

    function getAsset(bytes32 key) external view returns (Asset memory) {
        return _assets[key];
    }

    function isRegistered(uint64 chainId, address token) external view returns (bool) {
        return _assets[assetKey(chainId, token)].active;
    }

    function assetCount() external view returns (uint256) {
        return _assetKeys.length;
    }

    /// @notice Page through registered assets. Used by indexers to bootstrap their
    ///         scan set without replaying every `AssetRegistered` log.
    function listAssets(uint256 offset, uint256 limit) external view returns (Asset[] memory page) {
        uint256 n = _assetKeys.length;
        if (offset >= n) return new Asset[](0);
        uint256 end = offset + limit;
        if (end > n) end = n;
        page = new Asset[](end - offset);
        for (uint256 i = offset; i < end; i++) {
            page[i - offset] = _assets[_assetKeys[i]];
        }
    }

    // --------------------------------------------------------------------
    // Optimistic index commitments
    // --------------------------------------------------------------------

    /// @notice Publish a merkle commitment over the derived `account -> assets` index
    ///         for `[fromBlock, toBlock]` on `chainId`.
    function publishIndex(uint64 chainId, uint64 fromBlock, uint64 toBlock, bytes32 root, string calldata uri)
        external
        payable
        returns (uint256 epochId)
    {
        if (msg.value != publisherBond) revert BadBond();
        if (toBlock < fromBlock) revert BadRange();

        epochId = _epochs.length;
        _epochs.push(
            Epoch({
                chainId: chainId,
                fromBlock: fromBlock,
                toBlock: toBlock,
                root: root,
                uri: uri,
                publisher: msg.sender,
                challenger: address(0),
                bond: msg.value,
                publishedAt: uint64(block.timestamp),
                challengeDeadline: uint64(block.timestamp + challengeWindow),
                status: EpochStatus.Proposed
            })
        );

        emit IndexPublished(epochId, chainId, fromBlock, toBlock, root, uri, msg.sender);
    }

    /// @notice Bond a challenge against a proposed commitment before its window closes.
    /// @dev Adjudication is delegated to `arbiter`. A production deployment should replace
    ///      this with an optimistic oracle or an on-chain fraud proof over the source
    ///      chain's receipts; cross-chain log proofs are out of scope here.
    function challengeIndex(uint256 epochId) external payable {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        if (e.status != EpochStatus.Proposed) revert BadStatus();
        if (block.timestamp > e.challengeDeadline) revert WindowClosed();
        if (msg.value != e.bond) revert BadBond();

        e.status = EpochStatus.Challenged;
        e.challenger = msg.sender;
        e.bond += msg.value;

        emit IndexChallenged(epochId, msg.sender);
    }

    /// @notice Resolve a challenged commitment. `upheld == true` means the published root
    ///         was correct and the challenger forfeits.
    function resolveChallenge(uint256 epochId, bool upheld) external onlyArbiter {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        if (e.status != EpochStatus.Challenged) revert BadStatus();

        uint256 pot = e.bond;
        e.bond = 0;

        if (upheld) {
            e.status = EpochStatus.Finalized;
            _latestFinalized[e.chainId] = epochId + 1;
            emit IndexResolved(epochId, EpochStatus.Finalized);
            _pay(e.publisher, pot);
        } else {
            e.status = EpochStatus.Rejected;
            emit IndexResolved(epochId, EpochStatus.Rejected);
            _pay(e.challenger, pot);
        }
    }

    /// @notice Finalize an unchallenged commitment once its window has closed.
    function finalizeIndex(uint256 epochId) external {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        if (e.status != EpochStatus.Proposed) revert BadStatus();
        if (block.timestamp <= e.challengeDeadline) revert WindowOpen();

        uint256 refund = e.bond;
        e.bond = 0;
        e.status = EpochStatus.Finalized;
        _latestFinalized[e.chainId] = epochId + 1;

        emit IndexResolved(epochId, EpochStatus.Finalized);
        _pay(e.publisher, refund);
    }

    function epochCount() external view returns (uint256) {
        return _epochs.length;
    }

    function getEpoch(uint256 epochId) external view returns (Epoch memory) {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        return _epochs[epochId];
    }

    /// @notice Most recent finalized epoch for a chain.
    /// @return found False when the chain has no finalized epoch yet.
    function latestFinalizedEpoch(uint64 chainId) external view returns (bool found, uint256 epochId, Epoch memory e) {
        uint256 slot = _latestFinalized[chainId];
        if (slot == 0) return (false, 0, e);
        epochId = slot - 1;
        return (true, epochId, _epochs[epochId]);
    }

    // --------------------------------------------------------------------
    // Merkle verification
    // --------------------------------------------------------------------

    /// @notice Leaf encoding for the index commitment.
    /// @param assetsHash keccak256 over the ascending-sorted, packed asset addresses the
    ///        account touched on `chainId` within the epoch range.
    function leafHash(address account, uint64 chainId, bytes32 assetsHash) public pure returns (bytes32) {
        return keccak256(abi.encode(account, chainId, assetsHash));
    }

    /// @notice Verify an account's membership in a finalized (or proposed) commitment.
    /// @dev Sorted-pair merkle tree, matching the indexer's builder.
    function verifyInclusion(uint256 epochId, address account, bytes32 assetsHash, bytes32[] calldata proof)
        external
        view
        returns (bool)
    {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        bytes32 node = leafHash(account, e.chainId, assetsHash);
        for (uint256 i = 0; i < proof.length; i++) {
            bytes32 p = proof[i];
            node = node <= p ? keccak256(abi.encodePacked(node, p)) : keccak256(abi.encodePacked(p, node));
        }
        return node == e.root;
    }

    // --------------------------------------------------------------------
    // Admin
    // --------------------------------------------------------------------

    function setArbiter(address next) external onlyArbiter {
        emit ArbiterUpdated(arbiter, next);
        arbiter = next;
    }

    function setBonds(uint256 assetBond_, uint256 publisherBond_, uint256 challengeWindow_) external onlyArbiter {
        assetBond = assetBond_;
        publisherBond = publisherBond_;
        challengeWindow = challengeWindow_;
    }

    function _pay(address to, uint256 amount) private {
        if (amount == 0) return;
        (bool ok,) = to.call{value: amount}("");
        if (!ok) revert TransferFailed();
    }
}
