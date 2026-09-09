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
///     `account -> assets touched` table it derived for a block range, plus a second
///     root over the per-asset block ranges that table stands behind, plus a URI to
///     the full table. Both roots are *optimistic*: they finalize after a challenge
///     window unless someone bonds a challenge against them.
///
/// Money attaches to assets, not to chains. `requestIndexing` deposits funding on the
/// asset it names, and that funding is paid out per block of coverage: when an epoch
/// finalizes, its publisher claims `rewardPerBlock` for every block of an asset's
/// declared range that nobody has been paid for yet. A request therefore buys a
/// number of blocks of indexing for one contract, and a publisher is paid exactly
/// when it covers what someone paid to have covered. That is how the gas a publisher
/// fronts gets paid back: by the people who wanted the data, not by a sponsor the
/// contract has to trust.
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
///    Economics and the gateway list are therefore fixed at construction.
///
///  - **Local-arbiter mode** (`oracle == address(0)`): the pre-oracle fallback, for chains
///    with no oracle deployment. An `arbiter` address settles challenges. This mode is a
///    concession to reality, not the design: a deployment in it is only as neutral as
///    that one key, which is why `evmscan-deploy` prints the mode it is deploying in.
///
/// In both modes the bonds, the challenge window and the coverage pricing are immutable:
/// they are stated once in the constructor and no key can change them afterwards. The
/// arbiter cannot hand itself over either. The one thing a local arbiter may still edit
/// is the gateway list, which is a hint clients verify, never a rule.
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

    /// @notice What an asset has left to pay indexers, and which blocks have already
    ///         been paid for.
    struct Funding {
        /// @dev Wei available to pay coverage claims. Not refundable: it buys blocks.
        uint256 balance;
        /// @dev Contiguous block range already paid for. `paidTo == 0` means nothing
        ///      has been paid yet; block 0 is never a real coverage boundary.
        uint64 paidFrom;
        uint64 paidTo;
    }

    struct Epoch {
        uint64 chainId;
        uint64 fromBlock;
        uint64 toBlock;
        /// @dev Merkle root over leaves `keccak256(abi.encode(account, chainId, assetsHash))`.
        bytes32 root;
        /// @dev Merkle root over leaves `keccak256(abi.encode(assetKey, fromBlock, toBlock))`,
        ///      one per asset the publisher scanned, declaring the range it stands behind.
        bytes32 coverageRoot;
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

    /// @notice One asset's entry in a finalized epoch's coverage tree, with its proof.
    struct CoverageClaim {
        bytes32 key;
        uint64 fromBlock;
        uint64 toBlock;
        bytes32[] proof;
    }

    /// @notice The prices a deployment is configured with, grouped so the constructor
    ///         states them in one place. Each is also readable individually.
    struct Economics {
        /// @dev Wei `registerAsset` requires; refundable via `revokeAsset`.
        uint256 assetBond;
        /// @dev Per-commitment bond: wei in local-arbiter mode, `bondCurrency` units in
        ///      oracle mode.
        uint256 publisherBond;
        /// @dev Seconds a commitment stays disputable; assertion liveness in oracle mode.
        uint256 challengeWindow;
        /// @dev Least `requestIndexing` accepts above the asset bond.
        uint256 minFunding;
        /// @dev Paid per newly covered block of a funded asset.
        uint256 rewardPerBlock;
    }

    // --------------------------------------------------------------------
    // Storage
    // --------------------------------------------------------------------

    /// @notice UMA Optimistic Oracle V3, or `address(0)` in local-arbiter mode.
    IOptimisticOracleV3 public immutable oracle;
    /// @notice ERC-20 the oracle bonds are denominated in. Zero in local-arbiter mode.
    IERC20 public immutable bondCurrency;

    /// @notice Settles challenges in local-arbiter mode. Always zero in oracle mode.
    address public immutable arbiter;
    uint256 public immutable assetBond;
    /// @notice Bond a publisher posts per commitment: wei in local-arbiter mode, units of
    ///         `bondCurrency` in oracle mode.
    uint256 public immutable publisherBond;
    /// @notice Challenge window in seconds. In oracle mode this is the assertion liveness.
    uint256 public immutable challengeWindow;
    /// @notice Minimum funding `requestIndexing` accepts on top of the asset bond.
    uint256 public immutable minFunding;
    /// @notice Paid to a publisher per newly covered block of a funded asset.
    uint256 public immutable rewardPerBlock;

    mapping(bytes32 => Asset) private _assets;
    mapping(bytes32 => Funding) private _funding;
    bytes32[] private _assetKeys;

    Epoch[] private _epochs;
    /// @notice chainId => id of the most recent finalized epoch, +1 (0 means none).
    mapping(uint64 => uint256) private _latestFinalized;
    /// @notice assertionId => epoch id, +1 (0 means unknown).
    mapping(bytes32 => uint256) private _assertionEpoch;

    /// @notice ERC-3668 gateway URL templates (`{sender}` and `{data}` placeholders)
    ///         that serve leaves and proofs for `contractsOf`. Any number of
    ///         operators may run one; a client may also bring its own.
    string[] private _gateways;

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
        bytes32 coverageRoot,
        string uri,
        address publisher
    );
    /// @notice Oracle mode only: the commitment was asserted to the oracle.
    event IndexAsserted(uint256 indexed epochId, bytes32 indexed assertionId);
    event IndexChallenged(uint256 indexed epochId, address indexed challenger);
    event IndexResolved(uint256 indexed epochId, EpochStatus status);
    /// @notice Emitted once, at deployment. Everything in it is immutable, so this is the
    ///         whole configuration history of the contract.
    event RegistryConfigured(
        address indexed oracle,
        address indexed bondCurrency,
        address indexed arbiter,
        uint256 assetBond,
        uint256 publisherBond,
        uint256 challengeWindow,
        uint256 minFunding,
        uint256 rewardPerBlock
    );
    event AssetFunded(bytes32 indexed key, address indexed funder, uint256 amount, uint256 balance);
    event CoverageRewarded(
        uint256 indexed epochId,
        bytes32 indexed key,
        address indexed publisher,
        uint64 fromBlock,
        uint64 toBlock,
        uint64 newBlocks,
        uint256 reward
    );
    event GatewaysUpdated(string[] urls);

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
    error BadFee();
    error BadConfig();
    error BadRange();
    error BadStatus();
    error BadProof();
    error WindowOpen();
    error WindowClosed();
    error TransferFailed();
    error CurrencyTransferFailed();
    /// @notice The entry point exists only in local-arbiter mode.
    error OracleModeOnly();
    /// @dev ERC-3668: the answer lives off-chain; fetch it from `urls`, then call back.
    error OffchainLookup(address sender, string[] urls, bytes callData, bytes4 callbackFunction, bytes extraData);
    error NoFinalizedEpoch();
    error StaleEpoch();
    error Unsorted();

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
    /// @param econ_ Bonds, window and coverage pricing. See `Economics`.
    /// @param gateways_ Initial ERC-3668 gateway templates for `contractsOf`. In oracle
    ///        mode this list is final, because no setter is reachable.
    constructor(
        address oracle_,
        address bondCurrency_,
        address arbiter_,
        Economics memory econ_,
        string[] memory gateways_
    ) {
        if (econ_.challengeWindow == 0 || econ_.challengeWindow > type(uint64).max) revert BadConfig();

        if (oracle_ != address(0)) {
            // Oracle mode: no arbiter may exist, or the "neutral" deployment still has a
            // key that can settle disputes.
            if (arbiter_ != address(0) || bondCurrency_ == address(0)) revert BadConfig();
            // A bond the oracle would reject makes every publish revert; catch it here
            // rather than after deployment.
            if (econ_.publisherBond < IOptimisticOracleV3(oracle_).getMinimumBond(bondCurrency_)) revert BadBond();
        } else {
            if (arbiter_ == address(0) || bondCurrency_ != address(0)) revert BadConfig();
        }

        oracle = IOptimisticOracleV3(oracle_);
        bondCurrency = IERC20(bondCurrency_);
        arbiter = arbiter_;
        assetBond = econ_.assetBond;
        publisherBond = econ_.publisherBond;
        challengeWindow = econ_.challengeWindow;
        minFunding = econ_.minFunding;
        rewardPerBlock = econ_.rewardPerBlock;
        _setGateways(gateways_);

        emit RegistryConfigured(
            oracle_,
            bondCurrency_,
            arbiter_,
            econ_.assetBond,
            econ_.publisherBond,
            econ_.challengeWindow,
            econ_.minFunding,
            econ_.rewardPerBlock
        );
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
    ///      via `revokeAsset`. An asset registered this way has no funding, so nobody
    ///      is paid to index it; `requestIndexing` is the version that pays.
    function registerAsset(uint64 chainId, address token, uint8 kind, uint64 fromBlock)
        external
        payable
        returns (bytes32 key)
    {
        if (msg.value != assetBond) revert BadBond();
        key = assetKey(chainId, token);
        if (_assets[key].active) revert AlreadyRegistered();
        _register(key, chainId, token, kind, fromBlock, msg.value);
    }

    /// @notice Pay for a `(chainId, token)` pair to be indexed. Permissionless.
    /// @dev Registers the asset if it is not already, exactly as `registerAsset` does,
    ///      and deposits everything above the bond as that asset's funding. Funding is
    ///      paid out at `rewardPerBlock` per block of coverage a finalized epoch
    ///      declares for this asset, so a payment of `n * rewardPerBlock` buys `n`
    ///      blocks of indexing. For an asset that is already registered the whole
    ///      payment is funding.
    function requestIndexing(uint64 chainId, address token, uint8 kind, uint64 fromBlock)
        external
        payable
        returns (bytes32 key)
    {
        key = assetKey(chainId, token);
        uint256 funding = msg.value;
        if (!_assets[key].active) {
            if (msg.value < assetBond + minFunding) revert BadFee();
            funding = msg.value - assetBond;
            _register(key, chainId, token, kind, fromBlock, assetBond);
        } else if (msg.value < minFunding) {
            revert BadFee();
        }
        _fund(key, funding);
    }

    /// @notice Top up a registered asset's funding without touching its hint.
    /// @dev Where revenue from paid queries, or any other sponsor, lands.
    function fundAsset(bytes32 key) external payable {
        if (!_assets[key].active) revert UnknownAsset();
        _fund(key, msg.value);
    }

    function _fund(bytes32 key, uint256 amount) private {
        Funding storage f = _funding[key];
        f.balance += amount;
        emit AssetFunded(key, msg.sender, amount, f.balance);
    }

    function _register(bytes32 key, uint64 chainId, address token, uint8 kind, uint64 fromBlock, uint256 bond)
        private
    {
        // A revoked asset keeps its row and its place in the key list, so a re-register
        // must not add a second entry: `listAssets` is how indexers bootstrap their scan
        // set, and a duplicate there is a scan counted twice.
        bool known = _assets[key].registeredAt != 0;

        _assets[key] = Asset({
            chainId: chainId,
            token: token,
            kind: kind,
            fromBlock: fromBlock,
            registrant: msg.sender,
            registeredAt: uint64(block.timestamp),
            bond: bond,
            active: true
        });
        if (!known) _assetKeys.push(key);

        emit AssetRegistered(key, chainId, token, kind, fromBlock, msg.sender);
    }

    /// @notice Withdraw an asset hint and reclaim its bond.
    /// @dev Indexers keep whatever they already derived; this only stops future scanning.
    ///      Funding is not returned: it bought blocks, and an indexer may already have
    ///      scanned them on the strength of it. Whatever is left stays claimable.
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

    function getFunding(bytes32 key) external view returns (Funding memory) {
        return _funding[key];
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
    ///         for `[fromBlock, toBlock]` on `chainId`, together with the per-asset
    ///         coverage it stands behind.
    /// @dev In oracle mode the bond is `publisherBond` units of `bondCurrency`, pulled
    ///      from the caller (approve this contract first) and forwarded to the oracle;
    ///      `msg.value` must be zero. In local-arbiter mode the bond is `msg.value`.
    function publishIndex(
        uint64 chainId,
        uint64 fromBlock,
        uint64 toBlock,
        bytes32 root,
        bytes32 coverageRoot,
        string calldata uri
    ) external payable returns (uint256 epochId) {
        if (toBlock < fromBlock) revert BadRange();
        bool viaOracle = oracleMode();
        if (msg.value != (viaOracle ? 0 : publisherBond)) revert BadBond();

        epochId = _epochs.length;
        Epoch storage e = _epochs.push();
        e.chainId = chainId;
        e.fromBlock = fromBlock;
        e.toBlock = toBlock;
        e.root = root;
        e.coverageRoot = coverageRoot;
        e.uri = uri;
        e.publisher = msg.sender;
        e.bond = viaOracle ? publisherBond : msg.value;
        e.publishedAt = uint64(block.timestamp);
        e.challengeDeadline = uint64(block.timestamp + challengeWindow);
        e.status = EpochStatus.Proposed;

        _announce(epochId);
        if (viaOracle) _assert(epochId);
    }

    /// @dev Split out of `publishIndex` to keep its stack shallow; reads the epoch back
    ///      from storage so the event states exactly what was stored.
    function _announce(uint256 epochId) private {
        Epoch storage e = _epochs[epochId];
        emit IndexPublished(epochId, e.chainId, e.fromBlock, e.toBlock, e.root, e.coverageRoot, e.uri, e.publisher);
    }

    /// @dev Oracle mode: back the stored epoch with an assertion and remember the mapping.
    function _assert(uint256 epochId) private {
        bytes32 assertionId = _assertToOracle(epochId);
        _epochs[epochId].assertionId = assertionId;
        _assertionEpoch[assertionId] = epochId + 1;
        emit IndexAsserted(epochId, assertionId);
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
    ///
    ///      Finalizing returns the publisher's bond; the reward for the coverage the
    ///      epoch declared is claimed separately, per asset, with `claimCoverage`.
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
    // Coverage rewards
    // --------------------------------------------------------------------

    /// @notice Pay a finalized epoch's publisher for the coverage it declared.
    /// @dev Anyone may call this; the money always goes to the epoch's publisher. Each
    ///      claim proves one `(asset, fromBlock, toBlock)` leaf against the epoch's
    ///      coverage root and is paid `rewardPerBlock` for every block in that range
    ///      that no earlier claim on the asset has been paid for, capped by the
    ///      asset's funding. Replaying a claim, or claiming a range another epoch
    ///      already covered, pays nothing, so the number of epochs posted does not
    ///      change what a block of coverage is worth.
    ///
    ///      Gaps are paid as if covered: a claim of [100, 200] after [10, 20] was paid
    ///      pays for 21..200. The claim is a statement the publisher bonded and nobody
    ///      challenged, which is the same standing every other leaf in the epoch has.
    ///
    ///      A claim that pays nothing leaves the paid range alone, so covering an
    ///      unfunded asset is not forfeited: whoever funds it later pays for those
    ///      blocks. A claim the balance only partly covers marks the whole range paid;
    ///      funding is a cap, and the shortfall is the publisher's to accept or not.
    /// @return total Wei paid to the publisher across all claims.
    function claimCoverage(uint256 epochId, CoverageClaim[] calldata claims) external returns (uint256 total) {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        if (e.status != EpochStatus.Finalized) revert BadStatus();

        for (uint256 i = 0; i < claims.length; i++) {
            CoverageClaim calldata c = claims[i];
            if (c.fromBlock < e.fromBlock || c.toBlock > e.toBlock || c.toBlock < c.fromBlock || c.toBlock == 0) {
                revert BadRange();
            }
            if (_assets[c.key].chainId != e.chainId) revert UnknownAsset();
            if (!_verify(e.coverageRoot, coverageLeaf(c.key, c.fromBlock, c.toBlock), c.proof)) revert BadProof();

            Funding storage f = _funding[c.key];
            uint64 fresh = _fresh(f, c.fromBlock, c.toBlock);
            uint256 reward = uint256(fresh) * rewardPerBlock;
            if (reward > f.balance) reward = f.balance;
            if (reward > 0) {
                _extend(f, c.fromBlock, c.toBlock);
                f.balance -= reward;
                total += reward;
            }

            emit CoverageRewarded(epochId, c.key, e.publisher, c.fromBlock, c.toBlock, fresh, reward);
        }

        _pay(e.publisher, total);
    }

    /// @notice What `claimCoverage` would pay today for one asset range, before any
    ///         other claim moves the paid range. Lets a publisher decide whether an
    ///         epoch is worth the gas before posting it.
    function claimable(bytes32 key, uint64 fromBlock, uint64 toBlock) external view returns (uint256) {
        if (toBlock < fromBlock || toBlock == 0) return 0;
        Funding storage f = _funding[key];
        uint256 reward = uint256(_fresh(f, fromBlock, toBlock)) * rewardPerBlock;
        return reward > f.balance ? f.balance : reward;
    }

    /// @dev How many blocks of [fromBlock, toBlock] lie outside the paid range.
    function _fresh(Funding storage f, uint64 fromBlock, uint64 toBlock) private view returns (uint64 fresh) {
        if (f.paidTo == 0) return toBlock - fromBlock + 1;
        if (fromBlock < f.paidFrom) fresh += f.paidFrom - fromBlock;
        if (toBlock > f.paidTo) fresh += toBlock - f.paidTo;
    }

    /// @dev Grows the paid range to include [fromBlock, toBlock].
    function _extend(Funding storage f, uint64 fromBlock, uint64 toBlock) private {
        if (f.paidTo == 0) {
            f.paidFrom = fromBlock;
            f.paidTo = toBlock;
            return;
        }
        if (fromBlock < f.paidFrom) f.paidFrom = fromBlock;
        if (toBlock > f.paidTo) f.paidTo = toBlock;
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
    /// @param assetsDigest keccak256 over the ascending-sorted, packed asset addresses the
    ///        account touched on `chainId` within the epoch range.
    function leafHash(address account, uint64 chainId, bytes32 assetsDigest) public pure returns (bytes32) {
        return keccak256(abi.encode(account, chainId, assetsDigest));
    }

    /// @notice Leaf encoding for the coverage commitment.
    function coverageLeaf(bytes32 key, uint64 fromBlock, uint64 toBlock) public pure returns (bytes32) {
        return keccak256(abi.encode(key, fromBlock, toBlock));
    }

    /// @notice Verify an account's membership in a finalized (or proposed) commitment.
    /// @dev Sorted-pair merkle tree, matching the indexer's builder.
    function verifyInclusion(uint256 epochId, address account, bytes32 assetsDigest, bytes32[] calldata proof)
        external
        view
        returns (bool)
    {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        Epoch storage e = _epochs[epochId];
        return _verify(e.root, leafHash(account, e.chainId, assetsDigest), proof);
    }

    /// @notice Verify an asset range's membership in an epoch's coverage commitment.
    function verifyCoverage(uint256 epochId, bytes32 key, uint64 fromBlock, uint64 toBlock, bytes32[] calldata proof)
        external
        view
        returns (bool)
    {
        if (epochId >= _epochs.length) revert UnknownEpoch();
        return _verify(_epochs[epochId].coverageRoot, coverageLeaf(key, fromBlock, toBlock), proof);
    }

    function _verify(bytes32 root, bytes32 node, bytes32[] memory proof) private pure returns (bool) {
        for (uint256 i = 0; i < proof.length; i++) {
            bytes32 p = proof[i];
            node = node <= p ? keccak256(abi.encodePacked(node, p)) : keccak256(abi.encodePacked(p, node));
        }
        return node == root;
    }

    // --------------------------------------------------------------------
    // Verified lookups (ERC-3668, CCIP Read)
    // --------------------------------------------------------------------

    /// @notice Which contracts `account` has touched on `chainId`, per the latest
    ///         finalized commitment.
    /// @dev Always reverts with `OffchainLookup`. An ERC-3668 client fetches the
    ///      account's leaf and proof from any listed gateway and calls
    ///      `contractsOfCallback`, which checks them against the root stored here. A
    ///      gateway can therefore withhold an answer but cannot forge one, and the
    ///      callback refuses anything but the latest finalized epoch so it cannot
    ///      serve a stale one either.
    function contractsOf(uint64 chainId, address account) external view returns (address[] memory) {
        if (_latestFinalized[chainId] == 0) revert NoFinalizedEpoch();
        revert OffchainLookup(
            address(this),
            _gateways,
            abi.encodeWithSelector(this.contractsOf.selector, chainId, account),
            this.contractsOfCallback.selector,
            abi.encode(chainId, account)
        );
    }

    /// @notice ERC-3668 callback for `contractsOf`.
    /// @param response abi.encode(uint256 epochId, address[] assets, bytes32[] proof)
    /// @param extraData abi.encode(uint64 chainId, address account), as issued above
    function contractsOfCallback(bytes calldata response, bytes calldata extraData)
        external
        view
        returns (address[] memory assets)
    {
        (uint64 chainId, address account) = abi.decode(extraData, (uint64, address));
        uint256 epochId;
        bytes32[] memory proof;
        (epochId, assets, proof) = abi.decode(response, (uint256, address[], bytes32[]));

        uint256 slot = _latestFinalized[chainId];
        if (slot == 0) revert NoFinalizedEpoch();
        if (epochId != slot - 1) revert StaleEpoch();

        Epoch storage e = _epochs[epochId];
        if (!_verify(e.root, leafHash(account, chainId, assetsHash(assets)), proof)) revert BadProof();
    }

    /// @notice Digest of an account's asset list: keccak256 over the addresses packed
    ///         to 20 bytes each, which must be strictly ascending (sorted, unique).
    function assetsHash(address[] memory assets) public pure returns (bytes32) {
        bytes memory packed;
        for (uint256 i = 0; i < assets.length; i++) {
            if (i > 0 && assets[i] <= assets[i - 1]) revert Unsorted();
            packed = abi.encodePacked(packed, assets[i]);
        }
        return keccak256(packed);
    }

    function gateways() external view returns (string[] memory) {
        return _gateways;
    }

    // --------------------------------------------------------------------
    // Local-arbiter admin
    //
    // Bonds, window and pricing are immutable and the arbiter cannot be reassigned, so
    // this is the only entry point a key holds, and it reverts permanently in oracle
    // mode. Gateways are discovery hints: whatever they return is verified by
    // `contractsOfCallback`, and an ERC-3668 client may ignore the list and bring its
    // own, so the worst a bad list can do is make a lookup fail.
    // --------------------------------------------------------------------

    /// @notice Replace the gateway list `contractsOf` advertises.
    function setGateways(string[] memory urls) external onlyLocalArbiter {
        _setGateways(urls);
    }

    function _setGateways(string[] memory urls) private {
        delete _gateways;
        for (uint256 i = 0; i < urls.length; i++) {
            _gateways.push(urls[i]);
        }
        emit GatewaysUpdated(urls);
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
    function _assertToOracle(uint256 epochId) private returns (bytes32) {
        uint256 bond = publisherBond;
        _currencyCall(abi.encodeCall(IERC20.transferFrom, (msg.sender, address(this), bond)));
        _currencyCall(abi.encodeCall(IERC20.approve, (address(oracle), bond)));

        return oracle.assertTruth(
            _claim(epochId),
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
    ///      chain data to work from. Both roots are stated: the index root is what
    ///      consumers verify against, the coverage root is what the publisher gets paid
    ///      for, and a lie in either is grounds to reject.
    function _claim(uint256 epochId) private view returns (bytes memory) {
        Epoch storage e = _epochs[epochId];
        string memory uri = e.uri;
        return abi.encodePacked(
            "evm-scan index commitment asserted by ",
            _toHex(abi.encodePacked(e.publisher)),
            " at registry ",
            _toHex(abi.encodePacked(address(this))),
            ": epoch=",
            _toDecimal(epochId),
            " chainId=",
            _toDecimal(e.chainId),
            " fromBlock=",
            _toDecimal(e.fromBlock),
            " toBlock=",
            _toDecimal(e.toBlock),
            " root=",
            _toHex(abi.encodePacked(e.root)),
            " coverageRoot=",
            _toHex(abi.encodePacked(e.coverageRoot)),
            " uri=",
            uri,
            ". True if and only if root is the merkle root of the account-to-assets table"
            " derived from the source chain's logs over the inclusive block range, using the"
            " leaf encoding keccak256(abi.encode(account, chainId, assetsHash)) and a"
            " sorted-pair tree; coverageRoot is the merkle root, with the same tree rule, of"
            " leaves keccak256(abi.encode(assetKey, fromBlock, toBlock)) naming only asset"
            " ranges whose logs that table includes in full; and the full table is"
            " retrievable at uri."
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
