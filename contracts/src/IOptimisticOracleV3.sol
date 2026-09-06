// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice The subset of ERC-20 the registry needs to move assertion bonds.
interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function allowance(address owner, address spender) external view returns (uint256);
    function balanceOf(address account) external view returns (uint256);
}

/// @notice The subset of UMA's Optimistic Oracle V3 the registry calls.
///
/// Only the methods used by the "data asserter" pattern are declared: assert a claim
/// with a bond, let anyone dispute it, and settle once liveness has passed. The full
/// interface lives in UMA's protocol repo; narrowing it here keeps the registry
/// dependency-free and makes the trust surface obvious.
///
/// Reference: https://docs.uma.xyz/developers/optimistic-oracle-v3/data-asserter
interface IOptimisticOracleV3 {
    /// @notice Price identifier used for plain data assertions ("ASSERT_TRUTH").
    function defaultIdentifier() external view returns (bytes32);

    /// @notice Smallest bond the oracle accepts for `currency`. A bond below this is
    ///         rejected, because a dispute has to be worth more than its gas.
    function getMinimumBond(address currency) external view returns (uint256);

    /// @notice Assert that `claim` is true, bonding `bond` of `currency`.
    /// @dev The bond is pulled from `msg.sender`; `asserter` is who gets it back on a
    ///      successful settlement. `callbackRecipient` receives the dispute and
    ///      resolution callbacks.
    /// @return assertionId Identifier to dispute or settle this assertion.
    function assertTruth(
        bytes calldata claim,
        address asserter,
        address callbackRecipient,
        address escalationManager,
        uint64 liveness,
        IERC20 currency,
        uint256 bond,
        bytes32 identifier,
        bytes32 domainId
    ) external returns (bytes32 assertionId);

    /// @notice Dispute an assertion before its liveness expires. Matches the asserter's
    ///         bond, pulled from `msg.sender`, and escalates to UMA's DVM vote.
    function disputeAssertion(bytes32 assertionId, address disputer) external;

    /// @notice Settle an expired or resolved assertion and return its outcome. Reverts
    ///         while the assertion is still live and undisputed.
    function settleAndGetAssertionResult(bytes32 assertionId) external returns (bool);

    /// @notice Outcome of an already-settled assertion. Reverts if it is not settled.
    function getAssertionResult(bytes32 assertionId) external view returns (bool);
}

/// @notice Callbacks the oracle invokes on an assertion's `callbackRecipient`.
/// @dev Implemented by HintRegistry. Both are called by the oracle only.
interface IOptimisticOracleV3CallbackRecipient {
    function assertionResolvedCallback(bytes32 assertionId, bool assertedTruthfully) external;
    function assertionDisputedCallback(bytes32 assertionId) external;
}
