// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IERC20, IOptimisticOracleV3, IOptimisticOracleV3CallbackRecipient} from "./IOptimisticOracleV3.sol";

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
///
/// # Dispute adjudication
///
/// Commitments are adjudicated in one of two modes, fixed at deployment:
///
///  - **Oracle mode** (`oracle != address(0)`): `publishIndex` asserts the commitment to
///    UMA's Optimistic Oracle V3 with a bond in `bondCurrency`. Anyone disputes it
///    through the oracle, and UMA's DVM — not this contract — decides. There is no
///    arbiter, no owner, and no admin setter that can be reached: `arbiter` is
///    `address(0)`, so every `onlyLocalArbiter` entry point is permanently unreachable.
///
///  - **Local-arbiter mode** (`oracle == address(0)`): the pre-oracle fallback, for chains
///    with no oracle deployment. An `arbiter` address settles challenges. This mode is a
///    concession to reality, not the design: a deployment in it is only as neutral as
///    that one key, which is why `evmscan-deploy` prints the mode it is deploying in.
contract HintRegistry is IOptimisticOracleV3CallbackRecipient {
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
        /// @dev Set when a dispute is opened through `challengeIndex`. A dispute raised
        ///      directly at the oracle leaves this zero; the oracle records the disputer.
        address challenger;
        uint256 bond;
        uint64 publishedAt;
        uint64 challengeDeadline;
        EpochStatus status;
        /// @dev Oracle mode only: the OOv3 assertion backing this commitment.
        bytes32 assertionId;
    }

    // --------------------------------------------------------------------
    // Storage
    // --------------------------------------------------------------------

    /// @notice UMA Optimistic Oracle V3, or `address(0)` in local-arbiter mode.
    IOptimisticOracleV3 public immutable oracle;
    /// @notice ERC-20 the oracle bonds are denominated in. Zero in local-arbiter mode.
    IERC20 public immutable bondCurrency;

    /// @notice Settles challenges in local-arbiter mode. Always zero in oracle mode.
    address public arbiter;
    uint256 public assetBond;
    /// @notice Bond a publisher posts per commitment: wei in local-arbiter mode, units of
    ///         `bondCurrency` in oracle mode.
    uint256 public publisherBond;
    /// @notice Challenge window in seconds. In oracle mode this is the assertion liveness.
    uint256 public challengeWindow;

    mapping(bytes32 => Asset) private _assets;
    bytes32[] private _assetKeys;

    Epoch[] private _epochs;
    /// @notice chainId => id of the most recent finalized epoch, +1 (0 means none).
    mapping(uint64 => uint256) private _latestFinalized;
    /// @notice assertionId => epoch id, +1 (0 means unknown).
    mapping(bytes32 => uint256) private _assertionEpoch;

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
    /// @notice Oracle mode only: the commitment was asserted to the oracle.
    event IndexAsserted(uint256 indexed epochId, bytes32 indexed assertionId);
    event IndexChallenged(uint256 indexed epochId, address indexed challenger);
    event IndexResolved(uint256 indexed epochId, EpochStatus status);
    event ArbiterUpdated(address indexed previous, address indexed next);
    /// @notice Emitted once, at deployment, so the adjudication mode is on-chain history.
    event RegistryConfigured(
        address indexed oracle,
        address indexed bondCurrency,
        address indexed arbiter,
        uint256 assetBond,
        uint256 publisherBond,
        uint256 challengeWindow
    );

    // --------------------------------------------------------------------
    // Errors
    // --------------------------------------------------------------------

    error NotArbiter();
    error NotOracle();
    error NotRegistrant();
    error AlreadyRegistered();
    error UnknownAsset();
    error UnknownEpoch();
    error UnknownAssertion();
    error BadBond();
    error BadConfig();
    error BadRange();
    error BadStatus();
    error WindowOpen();
    error WindowClosed();
    error TransferFailed();
    error CurrencyTransferFailed();
    /// @notice The entry point exists only in local-arbiter mode.
    error OracleModeOnly();

    /// @dev Guards the entry points that only exist in local-arbiter mode. In oracle mode
    ///      `arbiter` is zero and `oracle` is not, so both branches are unreachable —
    ///      there is no admin path at all.
    modifier onlyLocalArbiter() {
        if (address(oracle) != address(0)) revert OracleModeOnly();
        if (msg.sender != arbiter) revert NotArbiter();
        _;
    }

    modifier onlyOracle() {
        if (msg.sender != address(oracle) || address(oracle) == address(0)) revert NotOracle();
        _;
    }

    /// @param oracle_ UMA Optimistic Oracle V3, or `address(0)` to fall back to a local
    ///        arbiter on a chain with no oracle deployment.
    /// @param bondCurrency_ ERC-20 for oracle bonds. Must be zero in local-arbiter mode.
    /// @param arbiter_ Local arbiter. Must be zero in oracle mode.
    /// @param challengeWindow_ Seconds a commitment stays disputable; the assertion
    ///        liveness in oracle mode.
    constructor(
        address oracle_,
        address bondCurrency_,
        address arbiter_,
        uint256 assetBond_,
        uint256 publisherBond_,
        uint256 challengeWindow_
    ) {
        if (challengeWindow_ == 0 || challengeWindow_ > type(uint64).max) revert BadConfig();

        if (oracle_ != address(0)) {
            // Oracle mode: no arbiter may exist, or the "neutral" deployment still has a
            // key that can settle disputes.
            if (arbiter_ != address(0) || bondCurrency_ == address(0)) revert BadConfig();
            // A bond the oracle would reject makes every publish revert; catch it here
            // rather than after deployment.
            if (publisherBond_ < IOptimisticOracleV3(oracle_).getMinimumBond(bondCurrency_)) revert BadBond();
        } else {
            if (arbiter_ == address(0) || bondCurrency_ != address(0)) revert BadConfig();
        }

        oracle = IOptimisticOracleV3(oracle_);
        bondCurrency = IERC20(bondCurrency_);
        arbiter = arbiter_;
        assetBond = assetBond_;
        publisherBond = publisherBond_;
        challengeWindow = challengeWindow_;

        if (arbiter_ != address(0)) emit ArbiterUpdated(address(0), arbiter_);
        emit RegistryConfigured(oracle_, bondCurrency_, arbiter_, assetBond_, publisherBond_, challengeWindow_);
    }

    /// @notice True when disputes are settled by the optimistic oracle rather than a key.
    function oracleMode() public view returns (bool) {
        return address(oracle) != address(0);
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
    /// @dev In oracle mode the bond is `publisherBond` units of `bondCurrency`, pulled
    ///      from the caller (approve this contract first) and forwarded to the oracle;
    ///      `msg.value` must be zero. In local-arbiter mode the bond is `msg.value`.
    function publishIndex(uint64 chainId, uint64 fromBlock, uint64 toBlock, bytes32 root, string calldata uri)
        external
        payable
        returns (uint256 epochId)
    {
        if (toBlock < fromBlock) revert BadRange();
        bool viaOracle = oracleMode();
        if (msg.value != (viaOracle ? 0 : publisherBond)) revert BadBond();

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
                bond: viaOracle ? publisherBond : msg.value,
                publishedAt: uint64(block.timestamp),
                challengeDeadline: uint64(block.timestamp + challengeWindow),
                status: EpochStatus.Proposed,
                assertionId: bytes32(0)
            })
        );

        emit IndexPublished(epochId, chainId, fromBlock, toBlock, root, uri, msg.sender);

        if (viaOracle) {
            bytes32 assertionId = _assertToOracle(epochId, chainId, fromBlock, toBlock, root, uri);
            _epochs[epochId].assertionId = assertionId;
            _assertionEpoch[assertionId] = epochId + 1;
            emit IndexAsserted(epochId, assertionId);
        }
    }

    /// @notice Dispute a proposed commitment before its window closes.
    /// @dev Oracle mode: a thin wrapper over `disputeAssertion`. The disputer's bond is
    ///      `publisherBond` units of `bondCurrency`, pulled from the caller (approve this
    ///      contract first), and UMA's DVM decides the outcome. Disputing at the oracle
    ///      directly works too and is equivalent; this path only exists so that consumers
    ///      have one address to talk to.
    ///      Local-arbiter mode: the challenge is bonded in wei and settled by `arbiter`.
    function challengeIndex(uint256 epochId) external payable {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        if (e.status != EpochStatus.Proposed) revert BadStatus();
        if (block.timestamp > e.challengeDeadline) revert WindowClosed();

        if (oracleMode()) {
            if (msg.value != 0) revert BadBond();
            // Recorded before the call: the oracle's dispute callback re-enters here and
            // emits the event with this address.
            e.challenger = msg.sender;
            uint256 bond = e.bond;
            _currencyCall(abi.encodeCall(IERC20.transferFrom, (msg.sender, address(this), bond)));
            _currencyCall(abi.encodeCall(IERC20.approve, (address(oracle), bond)));
            oracle.disputeAssertion(e.assertionId, msg.sender);
            // Belt and braces: a well-behaved oracle already flipped the status through
            // `assertionDisputedCallback`.
            if (e.status == EpochStatus.Proposed) {
                e.status = EpochStatus.Challenged;
                emit IndexChallenged(epochId, msg.sender);
            }
            return;
        }

        if (msg.value != e.bond) revert BadBond();
        e.status = EpochStatus.Challenged;
        e.challenger = msg.sender;
        e.bond += msg.value;

        emit IndexChallenged(epochId, msg.sender);
    }

    /// @notice Local-arbiter mode only: resolve a challenged commitment. `upheld == true`
    ///         means the published root was correct and the challenger forfeits.
    /// @dev Unreachable in oracle mode, where UMA's vote decides instead.
    function resolveChallenge(uint256 epochId, bool upheld) external onlyLocalArbiter {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        if (_epochs[epochId].status != EpochStatus.Challenged) revert BadStatus();
        _resolve(epochId, upheld);
    }

    /// @notice Finalize a commitment once its window has closed.
    /// @dev Oracle mode: settles the assertion at the oracle, which resolves the epoch
    ///      through the callback. Reverts while the assertion is still live or, once
    ///      disputed, until UMA has voted. Local-arbiter mode: finalizes an unchallenged
    ///      commitment after its deadline.
    function finalizeIndex(uint256 epochId) external {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];

        if (oracleMode()) {
            if (e.status != EpochStatus.Proposed && e.status != EpochStatus.Challenged) revert BadStatus();
            bool upheld = oracle.settleAndGetAssertionResult(e.assertionId);
            // The callback normally did this already; settle's own return value is the
            // same answer, so apply it if the callback was skipped.
            if (e.status == EpochStatus.Proposed || e.status == EpochStatus.Challenged) {
                _resolve(epochId, upheld);
            }
            return;
        }

        if (e.status != EpochStatus.Proposed) revert BadStatus();
        if (block.timestamp <= e.challengeDeadline) revert WindowOpen();
        _resolve(epochId, true);
    }

    // --------------------------------------------------------------------
    // Oracle callbacks
    // --------------------------------------------------------------------

    /// @inheritdoc IOptimisticOracleV3CallbackRecipient
    /// @dev Called by the oracle when an assertion expires undisputed or the DVM votes.
    function assertionResolvedCallback(bytes32 assertionId, bool assertedTruthfully) external onlyOracle {
        uint256 epochId = _epochForAssertion(assertionId);
        EpochStatus s = _epochs[epochId].status;
        // Already settled, e.g. through `finalizeIndex`'s fallback. Not an error.
        if (s != EpochStatus.Proposed && s != EpochStatus.Challenged) return;
        _resolve(epochId, assertedTruthfully);
    }

    /// @inheritdoc IOptimisticOracleV3CallbackRecipient
    /// @dev Called by the oracle when anyone disputes, including disputes raised at the
    ///      oracle rather than through `challengeIndex`.
    function assertionDisputedCallback(bytes32 assertionId) external onlyOracle {
        uint256 epochId = _epochForAssertion(assertionId);
        Epoch storage e = _epochs[epochId];
        if (e.status != EpochStatus.Proposed) return;
        e.status = EpochStatus.Challenged;
        emit IndexChallenged(epochId, e.challenger);
    }

    // --------------------------------------------------------------------
    // Views
    // --------------------------------------------------------------------

    function epochCount() external view returns (uint256) {
        return _epochs.length;
    }

    function getEpoch(uint256 epochId) external view returns (Epoch memory) {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        return _epochs[epochId];
    }

    /// @notice The oracle assertion backing a commitment. Zero in local-arbiter mode.
    function assertionOf(uint256 epochId) external view returns (bytes32) {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        return _epochs[epochId].assertionId;
    }

    /// @notice Reverse lookup from an oracle assertion to its commitment.
    function epochOfAssertion(bytes32 assertionId) external view returns (bool found, uint256 epochId) {
        uint256 slot = _assertionEpoch[assertionId];
        if (slot == 0) return (false, 0);
        return (true, slot - 1);
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
    // Local-arbiter admin
    //
    // Every function below reverts permanently in oracle mode.
    // --------------------------------------------------------------------

    function setArbiter(address next) external onlyLocalArbiter {
        if (next == address(0)) revert BadConfig();
        emit ArbiterUpdated(arbiter, next);
        arbiter = next;
    }

    function setBonds(uint256 assetBond_, uint256 publisherBond_, uint256 challengeWindow_) external onlyLocalArbiter {
        if (challengeWindow_ == 0 || challengeWindow_ > type(uint64).max) revert BadConfig();
        assetBond = assetBond_;
        publisherBond = publisherBond_;
        challengeWindow = challengeWindow_;
    }

    // --------------------------------------------------------------------
    // Internals
    // --------------------------------------------------------------------

    /// @dev Applies an outcome to a commitment. Bonds are only moved in local-arbiter
    ///      mode; in oracle mode the oracle custodies them and pays the winner directly.
    function _resolve(uint256 epochId, bool upheld) private {
        Epoch storage e = _epochs[epochId];
        uint256 pot = e.bond;
        e.bond = 0;

        address recipient;
        if (upheld) {
            e.status = EpochStatus.Finalized;
            // Only move the pointer forward: with an oracle, an older epoch can resolve
            // after a newer one, and consumers read this as "latest".
            if (epochId + 1 > _latestFinalized[e.chainId]) _latestFinalized[e.chainId] = epochId + 1;
            recipient = e.publisher;
        } else {
            e.status = EpochStatus.Rejected;
            recipient = e.challenger;
        }

        emit IndexResolved(epochId, e.status);
        if (!oracleMode()) _pay(recipient, pot);
    }

    /// @dev Asserts the commitment to the oracle, bonding `publisherBond` of
    ///      `bondCurrency` pulled from the publisher.
    function _assertToOracle(
        uint256 epochId,
        uint64 chainId,
        uint64 fromBlock,
        uint64 toBlock,
        bytes32 root,
        string calldata uri
    ) private returns (bytes32) {
        uint256 bond = publisherBond;
        _currencyCall(abi.encodeCall(IERC20.transferFrom, (msg.sender, address(this), bond)));
        _currencyCall(abi.encodeCall(IERC20.approve, (address(oracle), bond)));

        return oracle.assertTruth(
            _claim(epochId, chainId, fromBlock, toBlock, root, uri),
            msg.sender, // asserter: the publisher gets the bond back, not this contract
            address(this), // callbackRecipient
            address(0), // escalationManager: none, so anyone may dispute
            uint64(challengeWindow),
            bondCurrency,
            bond,
            oracle.defaultIdentifier(),
            bytes32(0) // domainId
        );
    }

    /// @dev The assertion text UMA voters read if the commitment is disputed. It has to
    ///      state the claim in full, because a voter has only this string and the public
    ///      chain data to work from.
    function _claim(
        uint256 epochId,
        uint64 chainId,
        uint64 fromBlock,
        uint64 toBlock,
        bytes32 root,
        string calldata uri
    ) private view returns (bytes memory) {
        return abi.encodePacked(
            "evm-scan index commitment asserted by ",
            _toHex(abi.encodePacked(msg.sender)),
            " at registry ",
            _toHex(abi.encodePacked(address(this))),
            ": epoch=",
            _toDecimal(epochId),
            " chainId=",
            _toDecimal(chainId),
            " fromBlock=",
            _toDecimal(fromBlock),
            " toBlock=",
            _toDecimal(toBlock),
            " root=",
            _toHex(abi.encodePacked(root)),
            " uri=",
            uri,
            ". True if and only if root is the merkle root of the account-to-assets table"
            " derived from the source chain's logs over the inclusive block range, using the"
            " leaf encoding keccak256(abi.encode(account, chainId, assetsHash)) and a"
            " sorted-pair tree, and the full table is retrievable at uri."
        );
    }

    /// @dev Calls `bondCurrency` and treats an empty return as success, so tokens that
    ///      predate the bool-returning ERC-20 signature still work.
    function _currencyCall(bytes memory data) private {
        (bool ok, bytes memory ret) = address(bondCurrency).call(data);
        if (!ok || (ret.length != 0 && !abi.decode(ret, (bool)))) revert CurrencyTransferFailed();
    }

    function _epochForAssertion(bytes32 assertionId) private view returns (uint256) {
        uint256 slot = _assertionEpoch[assertionId];
        if (slot == 0) revert UnknownAssertion();
        return slot - 1;
    }

    function _pay(address to, uint256 amount) private {
        if (amount == 0) return;
        (bool ok,) = to.call{value: amount}("");
        if (!ok) revert TransferFailed();
    }

    function _toDecimal(uint256 v) private pure returns (bytes memory) {
        if (v == 0) return "0";
        uint256 len;
        for (uint256 t = v; t != 0; t /= 10) len++;
        bytes memory out = new bytes(len);
        for (uint256 i = len; v != 0; v /= 10) {
            out[--i] = bytes1(uint8(48 + (v % 10)));
        }
        return out;
    }

    function _toHex(bytes memory raw) private pure returns (bytes memory) {
        bytes memory digits = "0123456789abcdef";
        bytes memory out = new bytes(2 + raw.length * 2);
        out[0] = "0";
        out[1] = "x";
        for (uint256 i = 0; i < raw.length; i++) {
            out[2 + i * 2] = digits[uint8(raw[i]) >> 4];
            out[3 + i * 2] = digits[uint8(raw[i]) & 0x0f];
        }
        return out;
    }
}
