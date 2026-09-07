// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// Stand-ins for the price sources PriceLens reads, shaped exactly like the real
// ones at the call sites the lens uses. They exist so the lens can be exercised on
// a chain that has no Chainlink and no Uniswap — a dev chain, or the in-process EVM
// in contracts/evmtest — through the same eth_call path production takes.
//
// Nothing here is for a real network. Every setter is open.

/// @notice Chainlink AggregatorV3Interface, with a knob.
contract MockAggregatorV3 {
    uint8 public immutable decimals;
    string public description;
    uint256 public constant version = 4;

    uint80 private _roundId;
    int256 private _answer;
    uint256 private _startedAt;
    uint256 private _updatedAt;

    constructor(uint8 decimals_, string memory description_, int256 answer_) {
        decimals = decimals_;
        description = description_;
        _set(answer_, block.timestamp);
    }

    /// @notice Post a new round, timestamped now.
    function setAnswer(int256 answer_) external {
        _set(answer_, block.timestamp);
    }

    /// @notice Post a round with an explicit timestamp, to fake a stale feed.
    function setAnswerAt(int256 answer_, uint256 updatedAt_) external {
        _set(answer_, updatedAt_);
    }

    function latestRoundData()
        external
        view
        returns (uint80 roundId, int256 answer, uint256 startedAt, uint256 updatedAt, uint80 answeredInRound)
    {
        return (_roundId, _answer, _startedAt, _updatedAt, _roundId);
    }

    function _set(int256 answer_, uint256 at) private {
        _roundId++;
        _answer = answer_;
        _startedAt = at;
        _updatedAt = at;
    }
}

/// @notice Chainlink Feed Registry: `getFeed(base, quote)`, reverting when unset, as
///         the real one does.
contract MockFeedRegistry {
    mapping(address => mapping(address => address)) private _feeds;

    error FeedNotFound();

    function setFeed(address base, address quote, address aggregator) external {
        _feeds[base][quote] = aggregator;
    }

    function getFeed(address base, address quote) external view returns (address) {
        address a = _feeds[base][quote];
        if (a == address(0)) revert FeedNotFound();
        return a;
    }
}

/// @notice Uniswap v3 factory surface: `getPool` for whatever pools were registered,
///         in either token order, as the real factory does.
contract MockUniswapV3Factory {
    mapping(address => mapping(address => mapping(uint24 => address))) public getPool;

    function register(address pool) external {
        MockUniswapV3Pool p = MockUniswapV3Pool(pool);
        (address t0, address t1, uint24 fee) = (p.token0(), p.token1(), p.fee());
        getPool[t0][t1][fee] = pool;
        getPool[t1][t0][fee] = pool;
    }
}

/// @notice The slice of a Uniswap v3 pool the lens reads: slot0, liquidity, the
///         observation ring buffer, and observe().
///
/// The oracle is modelled as "the tick has been constant since `since`": the
/// cumulative at time T is tick * T, so any window the lens asks for averages back
/// to exactly `tick`. Spot and TWAP are set independently so a test can make them
/// disagree.
contract MockUniswapV3Pool {
    address public immutable token0;
    address public immutable token1;
    uint24 public immutable fee;

    uint160 public sqrtPriceX96;
    int24 public tick;
    uint128 public liquidity;
    /// @dev Oldest observation the "ring buffer" holds. Windows longer than
    ///      block.timestamp - since are not observable, as on a real pool.
    uint32 public since;
    uint16 private _cardinality;

    error OLD();

    constructor(address token0_, address token1_, uint24 fee_, uint160 sqrtPriceX96_, int24 tick_, uint128 liquidity_) {
        require(token0_ < token1_, "token order");
        token0 = token0_;
        token1 = token1_;
        fee = fee_;
        sqrtPriceX96 = sqrtPriceX96_;
        tick = tick_;
        liquidity = liquidity_;
        since = uint32(block.timestamp);
        _cardinality = 1;
    }

    function setState(uint160 sqrtPriceX96_, int24 tick_, uint128 liquidity_) external {
        sqrtPriceX96 = sqrtPriceX96_;
        tick = tick_;
        liquidity = liquidity_;
    }

    /// @notice Pretend the oracle has history back to `since_`, with `cardinality`
    ///         slots in the buffer (0 models a pool whose oracle was never grown).
    function setHistory(uint32 since_, uint16 cardinality) external {
        since = since_;
        _cardinality = cardinality;
    }

    function slot0()
        external
        view
        returns (
            uint160 sqrtPriceX96_,
            int24 tick_,
            uint16 observationIndex,
            uint16 observationCardinality,
            uint16 observationCardinalityNext,
            uint8 feeProtocol,
            bool unlocked
        )
    {
        // Index 0 with cardinality 1 makes the "oldest" slot (index+1) % cardinality
        // == 0, which is initialised: the common case on a pool with a grown buffer
        // is exercised by a larger cardinality, where slot (index+1) is not.
        return (sqrtPriceX96, tick, 0, _cardinality, _cardinality, 0, true);
    }

    function observations(uint256 index)
        external
        view
        returns (uint32 blockTimestamp, int56 tickCumulative, uint160 secondsPerLiquidityCumulativeX128, bool initialized)
    {
        if (index == 0) return (since, int56(tick) * int56(uint56(since)), 0, true);
        return (0, 0, 0, false);
    }

    function observe(uint32[] calldata secondsAgos)
        external
        view
        returns (int56[] memory tickCumulatives, uint160[] memory secondsPerLiquidityCumulativeX128s)
    {
        tickCumulatives = new int56[](secondsAgos.length);
        secondsPerLiquidityCumulativeX128s = new uint160[](secondsAgos.length);
        for (uint256 i = 0; i < secondsAgos.length; i++) {
            uint32 target = uint32(block.timestamp) - secondsAgos[i];
            if (target < since) revert OLD();
            tickCumulatives[i] = int56(tick) * int56(uint56(target));
        }
    }
}

/// @notice Uniswap v2 factory surface: `getPair`, either order.
contract MockUniswapV2Factory {
    mapping(address => mapping(address => address)) public getPair;

    function register(address pair) external {
        MockUniswapV2Pair p = MockUniswapV2Pair(pair);
        (address t0, address t1) = (p.token0(), p.token1());
        getPair[t0][t1] = pair;
        getPair[t1][t0] = pair;
    }
}

/// @notice The slice of a Uniswap v2 pair the lens reads.
contract MockUniswapV2Pair {
    address public immutable token0;
    address public immutable token1;
    uint112 private _reserve0;
    uint112 private _reserve1;
    uint32 private _ts;

    constructor(address token0_, address token1_, uint112 reserve0_, uint112 reserve1_) {
        require(token0_ < token1_, "token order");
        token0 = token0_;
        token1 = token1_;
        _set(reserve0_, reserve1_);
    }

    function setReserves(uint112 reserve0_, uint112 reserve1_) external {
        _set(reserve0_, reserve1_);
    }

    function getReserves() external view returns (uint112 reserve0, uint112 reserve1, uint32 blockTimestampLast) {
        return (_reserve0, _reserve1, _ts);
    }

    function _set(uint112 r0, uint112 r1) private {
        _reserve0 = r0;
        _reserve1 = r1;
        _ts = uint32(block.timestamp);
    }
}
