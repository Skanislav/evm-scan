// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IERC20, IOptimisticOracleV3, IOptimisticOracleV3CallbackRecipient} from "./IOptimisticOracleV3.sol";

/// @title MockOptimisticOracleV3
/// @notice A dev-chain stand-in for UMA's Optimistic Oracle V3.
///
/// Local chains have no UMA deployment, so this exists purely so the registry's oracle
/// mode can be run end to end before it meets a real oracle: it keeps the same external
/// interface, the same bond mechanics and the same callback order, and replaces the DVM
/// vote with a single `voter` key that calls `resolveDispute`.
///
/// It is not a security model. Never deploy it anywhere that matters — in oracle mode a
/// registry is exactly as neutral as the oracle behind it, and this one is a single key.
contract MockOptimisticOracleV3 is IOptimisticOracleV3 {
    struct Assertion {
        bytes claim;
        address asserter;
        address callbackRecipient;
        address disputer;
        IERC20 currency;
        uint256 bond;
        uint64 expiresAt;
        bool settled;
        bool resolvedTruthful;
        bool voteCast;
        bool voteTruthful;
    }

    /// @notice Stands in for the DVM: resolves disputed assertions.
    address public immutable voter;
    uint256 public immutable minimumBond;

    mapping(bytes32 => Assertion) private _assertions;
    uint256 private _nonce;

    event AssertionMade(bytes32 indexed assertionId, address indexed asserter, uint64 expiresAt);
    event AssertionDisputed(bytes32 indexed assertionId, address indexed disputer);
    event AssertionResolved(bytes32 indexed assertionId, bool truthful);

    error NotVoter();
    error UnknownAssertion();
    error AlreadyDisputed();
    error AlreadySettled();
    error StillLive();
    error DisputeWindowClosed();
    error AwaitingVote();
    error BondTooSmall();
    error TransferFailed();

    constructor(address voter_, uint256 minimumBond_) {
        voter = voter_;
        minimumBond = minimumBond_;
    }

    function defaultIdentifier() external pure returns (bytes32) {
        return "ASSERT_TRUTH";
    }

    function getMinimumBond(address) external view returns (uint256) {
        return minimumBond;
    }

    function assertTruth(
        bytes calldata claim,
        address asserter,
        address callbackRecipient,
        address, /* escalationManager */
        uint64 liveness,
        IERC20 currency,
        uint256 bond,
        bytes32, /* identifier */
        bytes32 /* domainId */
    ) external returns (bytes32 assertionId) {
        if (bond < minimumBond) revert BondTooSmall();
        assertionId = keccak256(abi.encode(claim, asserter, block.timestamp, _nonce++));

        if (!currency.transferFrom(msg.sender, address(this), bond)) revert TransferFailed();

        _assertions[assertionId] = Assertion({
            claim: claim,
            asserter: asserter,
            callbackRecipient: callbackRecipient,
            disputer: address(0),
            currency: currency,
            bond: bond,
            expiresAt: uint64(block.timestamp) + liveness,
            settled: false,
            resolvedTruthful: false,
            voteCast: false,
            voteTruthful: false
        });

        emit AssertionMade(assertionId, asserter, _assertions[assertionId].expiresAt);
    }

    function disputeAssertion(bytes32 assertionId, address disputer) external {
        Assertion storage a = _assertions[assertionId];
        if (a.asserter == address(0)) revert UnknownAssertion();
        if (a.settled) revert AlreadySettled();
        if (a.disputer != address(0)) revert AlreadyDisputed();
        if (block.timestamp > a.expiresAt) revert DisputeWindowClosed();

        a.disputer = disputer;
        if (!a.currency.transferFrom(msg.sender, address(this), a.bond)) revert TransferFailed();

        emit AssertionDisputed(assertionId, disputer);
        if (a.callbackRecipient != address(0)) {
            IOptimisticOracleV3CallbackRecipient(a.callbackRecipient).assertionDisputedCallback(assertionId);
        }
    }

    /// @notice Stands in for a DVM vote on a disputed assertion.
    function resolveDispute(bytes32 assertionId, bool truthful) external {
        if (msg.sender != voter) revert NotVoter();
        Assertion storage a = _assertions[assertionId];
        if (a.asserter == address(0)) revert UnknownAssertion();
        if (a.disputer == address(0)) revert UnknownAssertion();
        if (a.settled) revert AlreadySettled();
        a.voteCast = true;
        a.voteTruthful = truthful;
    }

    function settleAndGetAssertionResult(bytes32 assertionId) external returns (bool) {
        Assertion storage a = _assertions[assertionId];
        if (a.asserter == address(0)) revert UnknownAssertion();
        if (a.settled) return a.resolvedTruthful;

        bool truthful;
        address winner;
        if (a.disputer == address(0)) {
            if (block.timestamp <= a.expiresAt) revert StillLive();
            truthful = true;
            winner = a.asserter;
        } else {
            if (!a.voteCast) revert AwaitingVote();
            truthful = a.voteTruthful;
            winner = truthful ? a.asserter : a.disputer;
        }

        a.settled = true;
        a.resolvedTruthful = truthful;

        // The real oracle burns a slice of the loser's bond; paying the whole pot keeps
        // the mock's accounting trivial without changing who wins.
        uint256 pot = a.disputer == address(0) ? a.bond : a.bond * 2;
        if (!a.currency.transfer(winner, pot)) revert TransferFailed();

        emit AssertionResolved(assertionId, truthful);
        if (a.callbackRecipient != address(0)) {
            IOptimisticOracleV3CallbackRecipient(a.callbackRecipient).assertionResolvedCallback(assertionId, truthful);
        }
        return truthful;
    }

    function getAssertionResult(bytes32 assertionId) external view returns (bool) {
        Assertion storage a = _assertions[assertionId];
        if (!a.settled) revert AwaitingVote();
        return a.resolvedTruthful;
    }

    function getAssertion(bytes32 assertionId) external view returns (Assertion memory) {
        return _assertions[assertionId];
    }
}
