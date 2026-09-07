// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ChainInfo} from "./AssetLens.sol";

// ---------------------------------------------------------------------------
// Wire types
//
// File-level, like AssetLens: the lens and the ABI-carrier interface both name
// them without inheritance putting runtime code into the lens.
// ---------------------------------------------------------------------------

/// @notice An explicit token -> Chainlink aggregator binding, for chains with no
///         Feed Registry or for feeds the operator wants to pin.
struct FeedHint {
    address token;
    /// @dev An AggregatorV3Interface (a Chainlink proxy, usually).
    address aggregator;
    /// @dev 0 = quoted in USD, 1 = quoted in the chain's native asset.
    uint8 quote;
}

/// @notice Everything one price read needs. Encoded as the constructor argument.
struct PriceRequest {
    /// @dev Tokens to price. Quote tokens the caller wants to route through must be
    ///      in here too: the lens prices each address independently and leaves the
    ///      routing (token -> WETH -> USD) to the client.
    address[] tokens;
    /// @dev Chainlink Feed Registry, or zero. Mainnet has one; most chains do not.
    address feedRegistry;
    /// @dev Aggregator for the native asset in USD (ETH/USD on Ethereum), or zero
    ///      to let the registry resolve it.
    address nativeUsdFeed;
    FeedHint[] feeds;
    /// @dev Uniswap v3 factory (or any getPool-compatible fork), or zero.
    address v3Factory;
    /// @dev Fee tiers to probe. Empty uses 100 / 500 / 3000 / 10000.
    uint24[] feeTiers;
    /// @dev Uniswap v2 factory (or any getPair-compatible fork), or zero.
    address v2Factory;
    /// @dev Pool counterparties to look for: the wrapped native asset and the
    ///      major stables. A token is priced against each of these it has a pool with.
    address[] quoteTokens;
    /// @dev Requested TWAP window in seconds. The lens reports the window it could
    ///      actually get from each pool's oracle, which may be shorter.
    uint32 twapWindow;
    /// @dev Gas ceiling per external call. 0 uses DEFAULT_CALL_GAS.
    uint256 gasPerCall;
    /// @dev Cap on every returned string. 0 uses DEFAULT_STRING_BYTES.
    uint256 maxStringBytes;
}

/// @notice One Chainlink-shaped feed, read once.
///
/// Nothing here is interpreted: the client decides what "stale" means and whether
/// an answer of zero is a price or a broken feed. The lens only reports what the
/// aggregator said and when it said it.
struct FeedInfo {
    address aggregator;
    /// @dev 0 USD, 1 native.
    uint8 quote;
    /// @dev Resolved through the Feed Registry rather than named by the caller.
    bool viaRegistry;
    /// @dev latestRoundData answered with a well-formed tuple.
    bool ok;
    int256 answer;
    uint8 decimals;
    bool hasDecimals;
    uint256 startedAt;
    uint256 updatedAt;
    uint80 roundId;
    uint80 answeredInRound;
    string description;
}

/// @notice One DEX pool holding the token against one of the quote tokens.
///
/// Raw observations only. Turning a tick into a price is arithmetic the client can
/// do exactly, and keeping it out of here keeps this contract small enough to ship
/// with every call.
struct PoolInfo {
    address pool;
    /// @dev 3 = Uniswap v3-style (concentrated liquidity, tick oracle),
    ///      2 = Uniswap v2-style (constant product, reserves only).
    uint8 kind;
    address token0;
    address token1;
    uint24 fee;
    // v3
    uint160 sqrtPriceX96;
    int24 tick;
    uint128 liquidity;
    /// @dev The TWAP window actually observed, in seconds. Zero when the pool's
    ///      oracle held no history to average over, in which case only the spot
    ///      price above is available and the client should say so.
    uint32 twapWindow;
    int56 tickCumulativeStart;
    int56 tickCumulativeEnd;
    // v2
    uint112 reserve0;
    uint112 reserve1;
    uint32 reserveTimestamp;
}

/// @notice Every price source the lens found for one token.
struct TokenPrices {
    address token;
    bool isContract;
    string symbol;
    uint8 decimals;
    bool hasDecimals;
    FeedInfo[] feeds;
    PoolInfo[] pools;
}

struct PriceResult {
    ChainInfo chain;
    /// @dev The native asset in USD. Everything quoted in ETH or routed through the
    ///      wrapped native token crosses through this one read.
    FeedInfo native;
    TokenPrices[] tokens;
}

/// @notice ABI carrier for the deployless reply, same trick as IAssetLens.
interface IPriceLens {
    function query(PriceRequest calldata req) external view returns (PriceResult memory res);
}

/// @title PriceLens
/// @notice Discovers and reads on-chain price sources for a set of tokens in a
///         single call, without being deployed.
///
/// ## What "price discovery" means here
///
/// evm-scan reads public state from your own node and asks nobody's permission. A
/// price is no different, as long as it comes from the chain rather than from a
/// quote API: Chainlink aggregators and DEX pools are contracts, and a snap-synced
/// node can read both at the head. The problem is finding them. For a token nobody
/// configured, this lens asks the chain itself:
///
///   1. the Chainlink Feed Registry, if the chain has one (mainnet does) —
///      `getFeed(token, USD)` and `getFeed(token, ETH)`;
///   2. any aggregator the operator pinned for the token;
///   3. every Uniswap v3 pool between the token and each quote token at each
///      fee tier, with the pool's own TWAP oracle read over the requested window;
///   4. every Uniswap v2 pair between the token and each quote token.
///
/// It returns all of them, raw, and the client picks: a fresh feed beats a TWAP
/// beats a spot, and a spot is labelled as the manipulable number it is.
///
/// ## Deployless, like AssetLens
///
/// The constructor returns `abi.encode(PriceResult)` and the client sends the
/// creation code as an `eth_call` with no `to`. Same limits (EIP-170 on the reply,
/// EIP-3860 on the payload), same batching on the client side, same posture: no
/// deployment, no address to trust, works on any chain the moment it compiles.
///
/// ## Reading hostile contracts
///
/// A factory, a pool or a feed is somebody else's code. Every read is a bounded
/// staticcall with capped gas and capped returndata; a source that reverts, lies
/// about its return shape or burns gas costs its own slot in the reply and nothing
/// else. Nothing a pool does can take the batch down.
contract PriceLens {
    // Chainlink AggregatorV3Interface + FeedRegistryInterface.
    bytes4 private constant SEL_LATEST_ROUND_DATA = bytes4(keccak256("latestRoundData()"));
    bytes4 private constant SEL_DECIMALS = bytes4(keccak256("decimals()"));
    bytes4 private constant SEL_DESCRIPTION = bytes4(keccak256("description()"));
    bytes4 private constant SEL_GET_FEED = bytes4(keccak256("getFeed(address,address)"));
    bytes4 private constant SEL_SYMBOL = bytes4(keccak256("symbol()"));

    // Uniswap v3 factory + pool.
    bytes4 private constant SEL_GET_POOL = bytes4(keccak256("getPool(address,address,uint24)"));
    bytes4 private constant SEL_TOKEN0 = bytes4(keccak256("token0()"));
    bytes4 private constant SEL_TOKEN1 = bytes4(keccak256("token1()"));
    bytes4 private constant SEL_LIQUIDITY = bytes4(keccak256("liquidity()"));
    bytes4 private constant SEL_SLOT0 = bytes4(keccak256("slot0()"));
    bytes4 private constant SEL_OBSERVATIONS = bytes4(keccak256("observations(uint256)"));
    bytes4 private constant SEL_OBSERVE = bytes4(keccak256("observe(uint32[])"));

    // Uniswap v2 factory + pair.
    bytes4 private constant SEL_GET_PAIR = bytes4(keccak256("getPair(address,address)"));
    bytes4 private constant SEL_GET_RESERVES = bytes4(keccak256("getReserves()"));

    /// @dev Chainlink's denomination sentinels, as the Feed Registry keys them.
    ///      USD is ISO 4217 code 840; ETH is the conventional 0xEeee… address.
    address private constant DENOM_USD = address(840);
    address private constant DENOM_ETH = 0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE;

    uint8 private constant QUOTE_USD = 0;
    uint8 private constant QUOTE_NATIVE = 1;
    uint8 private constant KIND_V2 = 2;
    uint8 private constant KIND_V3 = 3;

    uint256 private constant DEFAULT_CALL_GAS = 250_000;
    uint256 private constant DEFAULT_STRING_BYTES = 64;
    uint256 private constant MAX_STRING_BYTES = 256;
    uint256 private constant MAX_RETURN_BYTES = 1024;
    /// @dev Bounds the per-token pool sweep: quote tokens × (fee tiers + 1 v2 pair).
    uint256 private constant MAX_QUOTE_TOKENS = 8;
    uint256 private constant MAX_FEE_TIERS = 6;

    struct Ctx {
        address feedRegistry;
        FeedHint[] feeds;
        address v3Factory;
        uint24[] feeTiers;
        address v2Factory;
        address[] quoteTokens;
        uint32 twapWindow;
        uint256 gasPerCall;
        uint256 cap;
    }

    /// @notice Runs the query and returns `abi.encode(PriceResult)` as the "deployed code".
    constructor(PriceRequest memory req) {
        bytes memory out = abi.encode(_run(req));
        assembly {
            return(add(out, 0x20), mload(out))
        }
    }

    // ----------------------------------------------------------------------
    // Query
    // ----------------------------------------------------------------------

    function _run(PriceRequest memory req) private view returns (PriceResult memory res) {
        res.chain = ChainInfo({
            chainId: block.chainid,
            blockNumber: block.number,
            parentHash: block.number == 0 ? bytes32(0) : blockhash(block.number - 1),
            timestamp: block.timestamp,
            baseFee: block.basefee
        });

        uint256 cap = req.maxStringBytes == 0 ? DEFAULT_STRING_BYTES : req.maxStringBytes;
        Ctx memory c = Ctx({
            feedRegistry: req.feedRegistry,
            feeds: req.feeds,
            v3Factory: req.v3Factory,
            feeTiers: req.feeTiers.length == 0 ? _defaultFeeTiers() : _clipTiers(req.feeTiers),
            v2Factory: req.v2Factory,
            quoteTokens: _clipQuotes(req.quoteTokens),
            twapWindow: req.twapWindow,
            gasPerCall: req.gasPerCall == 0 ? DEFAULT_CALL_GAS : req.gasPerCall,
            cap: cap > MAX_STRING_BYTES ? MAX_STRING_BYTES : cap
        });

        // The native asset: an explicit feed wins, the registry's ETH/USD is the
        // fallback. Without either the reply says so with aggregator == 0.
        if (req.nativeUsdFeed != address(0)) {
            res.native = _readFeed(req.nativeUsdFeed, QUOTE_USD, false, c);
        } else if (c.feedRegistry != address(0)) {
            address agg = _getFeed(c.feedRegistry, DENOM_ETH, DENOM_USD, c.gasPerCall);
            if (agg != address(0)) res.native = _readFeed(agg, QUOTE_USD, true, c);
        }

        res.tokens = new TokenPrices[](req.tokens.length);
        for (uint256 i = 0; i < req.tokens.length; i++) {
            res.tokens[i] = _token(req.tokens[i], c);
        }
    }

    function _token(address token, Ctx memory c) private view returns (TokenPrices memory t) {
        t.token = token;
        t.isContract = _codeSize(token) > 0;
        t.feeds = new FeedInfo[](0);
        t.pools = new PoolInfo[](0);
        if (!t.isContract) return t;

        (bool okSym, bytes memory rSym) = _call(token, abi.encodeWithSelector(SEL_SYMBOL), c.gasPerCall);
        if (okSym) t.symbol = _string(rSym, c.cap);
        (t.decimals, t.hasDecimals) = _decimals(token, c.gasPerCall);

        t.feeds = _feeds(token, c);
        t.pools = _pools(token, c);
    }

    // ----------------------------------------------------------------------
    // Chainlink
    // ----------------------------------------------------------------------

    /// @dev Explicit hints first, then whatever the registry knows for the quotes the
    ///      hints did not cover. Two explicit hints for the same quote both come back;
    ///      choosing between them is the client's job.
    function _feeds(address token, Ctx memory c) private view returns (FeedInfo[] memory out) {
        FeedInfo[] memory buf = new FeedInfo[](c.feeds.length + 2);
        uint256 n;
        bool haveUsd;
        bool haveNative;

        for (uint256 i = 0; i < c.feeds.length; i++) {
            if (c.feeds[i].token != token) continue;
            uint8 q = c.feeds[i].quote == QUOTE_NATIVE ? QUOTE_NATIVE : QUOTE_USD;
            buf[n++] = _readFeed(c.feeds[i].aggregator, q, false, c);
            if (q == QUOTE_USD) haveUsd = true;
            else haveNative = true;
        }

        if (c.feedRegistry != address(0)) {
            if (!haveUsd) {
                address agg = _getFeed(c.feedRegistry, token, DENOM_USD, c.gasPerCall);
                if (agg != address(0)) buf[n++] = _readFeed(agg, QUOTE_USD, true, c);
            }
            if (!haveNative) {
                address agg = _getFeed(c.feedRegistry, token, DENOM_ETH, c.gasPerCall);
                if (agg != address(0)) buf[n++] = _readFeed(agg, QUOTE_NATIVE, true, c);
            }
        }

        out = new FeedInfo[](n);
        for (uint256 i = 0; i < n; i++) {
            out[i] = buf[i];
        }
    }

    /// @dev FeedRegistry.getFeed reverts when no feed exists, which the bounded call
    ///      turns into "zero address" rather than into a failed batch.
    function _getFeed(address registry, address base, address quote, uint256 gasPerCall)
        private
        view
        returns (address)
    {
        (bool ok, bytes memory r) =
            _call(registry, abi.encodeWithSelector(SEL_GET_FEED, base, quote), gasPerCall);
        if (!ok) return address(0);
        (uint256 v, bool okv) = _word(r, 0);
        return okv ? address(uint160(v)) : address(0);
    }

    function _readFeed(address agg, uint8 quote, bool viaRegistry, Ctx memory c)
        private
        view
        returns (FeedInfo memory f)
    {
        f.aggregator = agg;
        f.quote = quote;
        f.viaRegistry = viaRegistry;
        if (_codeSize(agg) == 0) return f;

        (bool ok, bytes memory r) = _call(agg, abi.encodeWithSelector(SEL_LATEST_ROUND_DATA), c.gasPerCall);
        if (ok && r.length >= 160) {
            (uint256 w0,) = _word(r, 0);
            (uint256 w1,) = _word(r, 1);
            (uint256 w2,) = _word(r, 2);
            (uint256 w3,) = _word(r, 3);
            (uint256 w4,) = _word(r, 4);
            f.roundId = uint80(w0);
            f.answer = int256(w1);
            f.startedAt = w2;
            f.updatedAt = w3;
            f.answeredInRound = uint80(w4);
            f.ok = true;
        }
        (f.decimals, f.hasDecimals) = _decimals(agg, c.gasPerCall);

        (bool okD, bytes memory rD) = _call(agg, abi.encodeWithSelector(SEL_DESCRIPTION), c.gasPerCall);
        if (okD) f.description = _string(rD, c.cap);
    }

    // ----------------------------------------------------------------------
    // DEX pools
    // ----------------------------------------------------------------------

    /// @dev Every (quote token, fee tier) v3 pool and every v2 pair that exists and
    ///      holds liquidity. Empty pools are dropped here rather than reported: a
    ///      tick with no liquidity behind it is not a price, and the reply has to
    ///      fit in 24KB.
    function _pools(address token, Ctx memory c) private view returns (PoolInfo[] memory out) {
        uint256 perQuote = (c.v3Factory == address(0) ? 0 : c.feeTiers.length) + (c.v2Factory == address(0) ? 0 : 1);
        PoolInfo[] memory buf = new PoolInfo[](c.quoteTokens.length * perQuote);
        uint256 n;

        for (uint256 q = 0; q < c.quoteTokens.length; q++) {
            address quote = c.quoteTokens[q];
            if (quote == token) continue;

            if (c.v3Factory != address(0)) {
                for (uint256 f = 0; f < c.feeTiers.length; f++) {
                    address pool = _lookup(
                        c.v3Factory, abi.encodeWithSelector(SEL_GET_POOL, token, quote, c.feeTiers[f]), c.gasPerCall
                    );
                    if (pool == address(0)) continue;
                    PoolInfo memory p = _readV3(pool, c.feeTiers[f], c);
                    if (_usable(p, token, quote)) buf[n++] = p;
                }
            }
            if (c.v2Factory != address(0)) {
                address pair = _lookup(c.v2Factory, abi.encodeWithSelector(SEL_GET_PAIR, token, quote), c.gasPerCall);
                if (pair != address(0)) {
                    PoolInfo memory p = _readV2(pair, c.gasPerCall);
                    if (_usable(p, token, quote)) buf[n++] = p;
                }
            }
        }

        out = new PoolInfo[](n);
        for (uint256 i = 0; i < n; i++) {
            out[i] = buf[i];
        }
    }

    /// @dev A pool is only reported when it really is between the token and the
    ///      quote (a factory that hands back an unrelated address is lying) and
    ///      when there is something in it.
    function _usable(PoolInfo memory p, address token, address quote) private pure returns (bool) {
        bool pair = (p.token0 == token && p.token1 == quote) || (p.token0 == quote && p.token1 == token);
        if (!pair) return false;
        if (p.kind == KIND_V3) return p.liquidity > 0 && p.sqrtPriceX96 > 0;
        return p.reserve0 > 0 && p.reserve1 > 0;
    }

    function _lookup(address factory, bytes memory data, uint256 gasPerCall) private view returns (address) {
        (bool ok, bytes memory r) = _call(factory, data, gasPerCall);
        if (!ok) return address(0);
        (uint256 v, bool okv) = _word(r, 0);
        if (!okv) return address(0);
        address a = address(uint160(v));
        return _codeSize(a) > 0 ? a : address(0);
    }

    function _readV3(address pool, uint24 fee, Ctx memory c) private view returns (PoolInfo memory p) {
        p.pool = pool;
        p.kind = KIND_V3;
        p.fee = fee;
        p.token0 = _addressOf(pool, SEL_TOKEN0, c.gasPerCall);
        p.token1 = _addressOf(pool, SEL_TOKEN1, c.gasPerCall);

        (bool okL, bytes memory rL) = _call(pool, abi.encodeWithSelector(SEL_LIQUIDITY), c.gasPerCall);
        if (okL) {
            (uint256 l,) = _word(rL, 0);
            p.liquidity = uint128(l);
        }

        (bool ok, bytes memory r) = _call(pool, abi.encodeWithSelector(SEL_SLOT0), c.gasPerCall);
        if (!ok || r.length < 224) return p;
        (uint256 w0,) = _word(r, 0);
        (uint256 w1,) = _word(r, 1);
        (uint256 w2,) = _word(r, 2);
        (uint256 w3,) = _word(r, 3);
        p.sqrtPriceX96 = uint160(w0);
        p.tick = int24(int256(w1));
        uint16 index = uint16(w2);
        uint16 cardinality = uint16(w3);

        if (c.twapWindow > 0 && cardinality > 0) {
            _twap(p, index, cardinality, c);
        }
    }

    /// @dev The pool's oracle can only average over the history it kept. Find the
    ///      oldest observation the ring buffer still holds, clamp the window to it,
    ///      and read the cumulatives at both ends. A pool whose oldest observation
    ///      is the current block yields no window at all, and says so with 0.
    function _twap(PoolInfo memory p, uint16 index, uint16 cardinality, Ctx memory c) private view {
        (uint32 oldestTs, bool okTs) = _oldestObservation(p.pool, index, cardinality, c.gasPerCall);
        if (!okTs) return;

        uint32 age;
        unchecked {
            age = uint32(block.timestamp) - oldestTs;
        }
        uint32 window = c.twapWindow < age ? c.twapWindow : age;
        if (window == 0) return;

        (int56 c0, int56 c1, bool ok) = _observe(p.pool, window, c.gasPerCall);
        if (!ok) return;
        p.twapWindow = window;
        p.tickCumulativeStart = c0;
        p.tickCumulativeEnd = c1;
    }

    /// @dev Timestamp of the oldest observation in the ring buffer: the slot after
    ///      the current one once the buffer has wrapped, slot 0 until then.
    function _oldestObservation(address pool, uint16 index, uint16 cardinality, uint256 gasPerCall)
        private
        view
        returns (uint32 ts, bool ok)
    {
        uint256 oldest = (uint256(index) + 1) % cardinality;
        (bool okCall, bytes memory r) = _call(pool, abi.encodeWithSelector(SEL_OBSERVATIONS, oldest), gasPerCall);
        if (!okCall || r.length < 128) return (0, false);
        (uint256 w0,) = _word(r, 0);
        (uint256 initialized,) = _word(r, 3);
        if (initialized != 0) return (uint32(w0), true);

        (okCall, r) = _call(pool, abi.encodeWithSelector(SEL_OBSERVATIONS, uint256(0)), gasPerCall);
        if (!okCall || r.length < 128) return (0, false);
        (w0,) = _word(r, 0);
        return (uint32(w0), true);
    }

    /// @dev observe([window, 0]) decoded tolerantly: the reply is
    ///      (int56[] tickCumulatives, uint160[] secondsPerLiquidityCumulativeX128s),
    ///      and only the first array's two members are wanted.
    function _observe(address pool, uint32 window, uint256 gasPerCall)
        private
        view
        returns (int56 c0, int56 c1, bool ok)
    {
        uint32[] memory ago = new uint32[](2);
        ago[0] = window;
        ago[1] = 0;
        (bool okCall, bytes memory r) = _call(pool, abi.encodeWithSelector(SEL_OBSERVE, ago), gasPerCall);
        if (!okCall) return (0, 0, false);

        (uint256 off, bool okOff) = _word(r, 0);
        if (!okOff || off % 32 != 0 || off + 96 > r.length) return (0, 0, false);
        (uint256 len,) = _word(r, off / 32);
        if (len < 2) return (0, 0, false);
        (uint256 w1,) = _word(r, off / 32 + 1);
        (uint256 w2,) = _word(r, off / 32 + 2);
        return (int56(int256(w1)), int56(int256(w2)), true);
    }

    function _readV2(address pair, uint256 gasPerCall) private view returns (PoolInfo memory p) {
        p.pool = pair;
        p.kind = KIND_V2;
        p.token0 = _addressOf(pair, SEL_TOKEN0, gasPerCall);
        p.token1 = _addressOf(pair, SEL_TOKEN1, gasPerCall);

        (bool ok, bytes memory r) = _call(pair, abi.encodeWithSelector(SEL_GET_RESERVES), gasPerCall);
        if (!ok || r.length < 96) return p;
        (uint256 w0,) = _word(r, 0);
        (uint256 w1,) = _word(r, 1);
        (uint256 w2,) = _word(r, 2);
        p.reserve0 = uint112(w0);
        p.reserve1 = uint112(w1);
        p.reserveTimestamp = uint32(w2);
    }

    // ----------------------------------------------------------------------
    // Request normalisation
    // ----------------------------------------------------------------------

    function _defaultFeeTiers() private pure returns (uint24[] memory tiers) {
        tiers = new uint24[](4);
        tiers[0] = 100;
        tiers[1] = 500;
        tiers[2] = 3000;
        tiers[3] = 10000;
    }

    function _clipTiers(uint24[] memory tiers) private pure returns (uint24[] memory) {
        if (tiers.length <= MAX_FEE_TIERS) return tiers;
        uint24[] memory out = new uint24[](MAX_FEE_TIERS);
        for (uint256 i = 0; i < MAX_FEE_TIERS; i++) {
            out[i] = tiers[i];
        }
        return out;
    }

    function _clipQuotes(address[] memory quotes) private pure returns (address[] memory) {
        if (quotes.length <= MAX_QUOTE_TOKENS) return quotes;
        address[] memory out = new address[](MAX_QUOTE_TOKENS);
        for (uint256 i = 0; i < MAX_QUOTE_TOKENS; i++) {
            out[i] = quotes[i];
        }
        return out;
    }

    // ----------------------------------------------------------------------
    // Bounded calls and tolerant decoding
    // ----------------------------------------------------------------------

    function _codeSize(address a) private view returns (uint256 size) {
        assembly ("memory-safe") {
            size := extcodesize(a)
        }
    }

    function _decimals(address target, uint256 gasPerCall) private view returns (uint8 d, bool ok) {
        (bool okCall, bytes memory r) = _call(target, abi.encodeWithSelector(SEL_DECIMALS), gasPerCall);
        if (!okCall) return (0, false);
        (uint256 v, bool okv) = _word(r, 0);
        // Nothing honest scales by more than 10**77; anything above is garbage.
        if (!okv || v > 77) return (0, false);
        return (uint8(v), true);
    }

    function _addressOf(address target, bytes4 sel, uint256 gasPerCall) private view returns (address) {
        (bool ok, bytes memory r) = _call(target, abi.encodeWithSelector(sel), gasPerCall);
        if (!ok) return address(0);
        (uint256 v, bool okv) = _word(r, 0);
        return okv ? address(uint160(v)) : address(0);
    }

    /// @dev staticcall with capped gas and capped returndata copy. Never reverts.
    function _call(address target, bytes memory data, uint256 gasLimit)
        private
        view
        returns (bool ok, bytes memory ret)
    {
        uint256 maxReturn = MAX_RETURN_BYTES;
        assembly {
            ok := staticcall(gasLimit, target, add(data, 0x20), mload(data), 0, 0)
            let size := returndatasize()
            if gt(size, maxReturn) { size := maxReturn }
            ret := mload(0x40)
            mstore(ret, size)
            returndatacopy(add(ret, 0x20), 0, size)
            mstore(0x40, add(ret, and(add(add(size, 0x20), 0x1f), not(0x1f))))
        }
    }

    /// @dev Word `i` of a return buffer, or (0, false) when the buffer is too short.
    function _word(bytes memory ret, uint256 i) private pure returns (uint256 v, bool ok) {
        if (ret.length < 32 * (i + 1)) return (0, false);
        assembly {
            v := mload(add(add(ret, 0x20), mul(i, 32)))
        }
        ok = true;
    }

    /// @dev ABI string or legacy bytes32, truncated to `cap`, offsets never trusted.
    function _string(bytes memory ret, uint256 cap) private pure returns (string memory s) {
        if (ret.length == 32) {
            uint256 n;
            while (n < 32 && ret[n] != 0) {
                n++;
            }
            if (n > cap) n = cap;
            bytes memory b = new bytes(n);
            for (uint256 i = 0; i < n; i++) {
                b[i] = ret[i];
            }
            return string(b);
        }
        if (ret.length < 64) return "";

        (uint256 off,) = _word(ret, 0);
        if (off > ret.length || ret.length - off < 32) return "";
        uint256 len;
        assembly {
            len := mload(add(add(ret, 0x20), off))
        }
        uint256 avail = ret.length - off - 32;
        if (len > avail) len = avail;
        if (len > cap) len = cap;
        bytes memory out = new bytes(len);
        for (uint256 i = 0; i < len; i++) {
            out[i] = ret[off + 32 + i];
        }
        return string(out);
    }
}
