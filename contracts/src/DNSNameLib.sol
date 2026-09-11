// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title DNSNameLib
/// @notice Reads ENS names in DNS wire format (ENSIP-10) and parses the two label
///         shapes HintResolver understands: a hex account address and a decimal chain
///         id. Pure, allocation-light, no dependencies.
library DNSNameLib {
    /// @notice Reads the label starting at `offset` of a DNS-encoded name.
    /// @return label The label bytes (empty at the terminating zero byte).
    /// @return next The offset of the following label.
    function readLabel(bytes memory name, uint256 offset) internal pure returns (bytes memory label, uint256 next) {
        if (offset >= name.length) return (label, name.length);
        uint256 len = uint8(name[offset]);
        if (len == 0) return (label, offset + 1);
        require(offset + 1 + len <= name.length, "DNSNameLib: truncated");
        label = new bytes(len);
        for (uint256 i = 0; i < len; i++) {
            label[i] = name[offset + 1 + i];
        }
        next = offset + 1 + len;
    }

    /// @notice Parses a label as a 20-byte address: 40 hex characters, or 42 with a
    ///         `0x` prefix, in either case. Nothing else is an account.
    function parseHexAddress(bytes memory label) internal pure returns (address account, bool ok) {
        uint256 start = 0;
        if (label.length == 42) {
            if (label[0] != "0" || (label[1] != "x" && label[1] != "X")) return (address(0), false);
            start = 2;
        } else if (label.length != 40) {
            return (address(0), false);
        }
        uint160 acc = 0;
        for (uint256 i = start; i < label.length; i++) {
            (uint8 nibble, bool valid) = hexNibble(label[i]);
            if (!valid) return (address(0), false);
            acc = (acc << 4) | nibble;
        }
        return (address(acc), true);
    }

    /// @notice Parses a label of decimal digits that fits a uint64.
    function parseDecimal(bytes memory label) internal pure returns (uint64 value, bool ok) {
        if (label.length == 0 || label.length > 20) return (0, false);
        uint256 acc = 0;
        for (uint256 i = 0; i < label.length; i++) {
            uint8 c = uint8(label[i]);
            if (c < 0x30 || c > 0x39) return (0, false);
            acc = acc * 10 + (c - 0x30);
            if (acc > type(uint64).max) return (0, false);
        }
        return (uint64(acc), true);
    }

    function hexNibble(bytes1 c) internal pure returns (uint8, bool) {
        uint8 b = uint8(c);
        if (b >= 0x30 && b <= 0x39) return (b - 0x30, true);
        if (b >= 0x61 && b <= 0x66) return (b - 0x61 + 10, true);
        if (b >= 0x41 && b <= 0x46) return (b - 0x41 + 10, true);
        return (0, false);
    }

    bytes16 private constant HEX = "0123456789abcdef";

    /// @notice Lowercase `0x`-prefixed hex of an address.
    function toHexString(address a) internal pure returns (string memory) {
        bytes memory out = new bytes(42);
        out[0] = "0";
        out[1] = "x";
        uint160 v = uint160(a);
        for (uint256 i = 41; i > 1; i--) {
            out[i] = HEX[v & 0xf];
            v >>= 4;
        }
        return string(out);
    }

    /// @notice Lowercase `0x`-prefixed hex of a 32-byte word.
    function toHexString(bytes32 h) internal pure returns (string memory) {
        bytes memory out = new bytes(66);
        out[0] = "0";
        out[1] = "x";
        uint256 v = uint256(h);
        for (uint256 i = 65; i > 1; i--) {
            out[i] = HEX[v & 0xf];
            v >>= 4;
        }
        return string(out);
    }

    /// @notice Decimal rendering of an unsigned integer.
    function toDecimalString(uint256 v) internal pure returns (string memory) {
        if (v == 0) return "0";
        uint256 digits;
        for (uint256 t = v; t != 0; t /= 10) digits++;
        bytes memory out = new bytes(digits);
        while (v != 0) {
            out[--digits] = bytes1(uint8(0x30 + (v % 10)));
            v /= 10;
        }
        return string(out);
    }
}
