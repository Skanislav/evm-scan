// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// ---------------------------------------------------------------------------
// Wire types
//
// These are file-level so both the lens itself and the ABI-carrier interface
// below can name them without inheritance forcing runtime code into the lens.
// ---------------------------------------------------------------------------

/// @notice One contract to read, plus the token ids to look at inside it.
struct TokenQuery {
    address token;
    /// @dev ERC-721 ids to resolve (ownerOf/getApproved/tokenURI) or ERC-1155 ids to
    ///      balance. Leave empty to read only the contract-level facts.
    uint256[] ids;
}

/// @notice Everything one lens call reads. Encoded as the constructor argument.
struct Request {
    /// @dev The subject: whose balances, allowances and account state to report.
    address account;
    /// @dev ERC-20 allowance(account, spender) and ERC-721/1155
    ///      isApprovedForAll(account, spender) are read for each of these.
    address[] spenders;
    TokenQuery[] tokens;
    /// @dev tokenURI/uri are long and rarely needed; opt in.
    bool includeUri;
    /// @dev Return the account's full code. The 7702 delegation target comes back
    ///      either way, so this is only for callers that want the bytecode itself.
    bool includeCode;
    /// @dev For ERC-721Enumerable contracts with no explicit ids, walk up to this
    ///      many of the account's tokens via tokenOfOwnerByIndex. 0 disables it.
    uint256 enumerateLimit;
    /// @dev Gas ceiling per external call. 0 uses DEFAULT_CALL_GAS. One hostile
    ///      token must not be able to burn the whole eth_call budget.
    uint256 gasPerCall;
    /// @dev Cap on every returned string. 0 uses DEFAULT_STRING_BYTES. The reply has
    ///      to fit in EIP-170's 24576 bytes (see AssetLens), so strings are bounded
    ///      here rather than trusted.
    uint256 maxStringBytes;
}

/// @notice The block the whole reply is as of.
///
/// Every read below happens in one EVM execution, so this is not "roughly now" the
/// way a sequence of eth_calls is: the entire result is a consistent snapshot of
/// this block, and the caller does not have to ask for the head separately and hope
/// nothing moved in between.
struct ChainInfo {
    uint256 chainId;
    uint256 blockNumber;
    /// @dev Hash of blockNumber-1. The current block's hash does not exist yet from
    ///      inside the EVM, not even for an eth_call against a sealed block.
    bytes32 parentHash;
    uint256 timestamp;
    uint256 baseFee;
}

/// @notice The subject account's own state.
///
/// Note what is missing: the transaction nonce. The EVM has no opcode for another
/// account's nonce, so no contract — deployless or not — can report it. It comes
/// from eth_getTransactionCount alongside this call; see internal/lens.
struct AccountInfo {
    address account;
    /// @dev Native balance in wei.
    uint256 balance;
    /// @dev EXTCODEHASH: zero for an account that does not exist, and
    ///      keccak256("") for one that exists with no code.
    bytes32 codeHash;
    uint256 codeSize;
    /// @dev Code that is not an EIP-7702 delegation designator.
    bool isContract;
    /// @dev EIP-7702: code is exactly 0xef0100 || address.
    bool isDelegated;
    /// @dev The delegation target, or zero when not delegated.
    address delegate;
    /// @dev Only when Request.includeCode.
    bytes code;
}

/// @notice One token id's worth of NFT state.
struct TokenIdInfo {
    uint256 id;
    /// @dev ERC-721 ownerOf.
    address owner;
    bool ownerKnown;
    /// @dev ERC-721: 1 when the account owns it. ERC-1155: balanceOf(account, id).
    uint256 balance;
    bool balanceKnown;
    /// @dev ERC-721 getApproved.
    address approved;
    bool approvedKnown;
    /// @dev tokenURI/uri, when Request.includeUri. Truncated to maxStringBytes.
    string uri;
    bool uriTruncated;
}

/// @notice What one contract says about itself and about the account.
///
/// Every field carries its own "known" flag rather than a zero standing in for both
/// "no" and "did not answer". A token that reverts on decimals() is not a token with
/// zero decimals, and a wallet showing 0 because a call failed is a bug.
struct TokenInfo {
    address token;
    bool isContract;
    /// @dev 0 unknown, 20 ERC-20, 21 ERC-721, 55 ERC-1155 — the same sentinels the
    ///      indexer and HintRegistry use (721 and 1155 do not fit in a uint8).
    uint8 standard;
    bool supportsErc165;
    bool isErc721;
    bool isErc1155;
    bool isEnumerable;
    string symbol;
    string name;
    uint8 decimals;
    bool hasDecimals;
    uint256 totalSupply;
    bool hasTotalSupply;
    /// @dev balanceOf(account): units for ERC-20, held count for ERC-721.
    uint256 balance;
    bool hasBalance;
    /// @dev Per Request.spenders, in order.
    uint256[] allowances;
    bool[] allowanceKnown;
    bool[] approvedForAll;
    TokenIdInfo[] ids;
}

struct Result {
    ChainInfo chain;
    AccountInfo account;
    TokenInfo[] tokens;
}

/// @notice ABI carrier for the deployless reply.
///
/// The lens returns `abi.encode(Result)` from its constructor, which is exactly how
/// a `query(Request) returns (Result)` call would encode it. Declaring that function
/// on an interface — never implemented, so it costs no bytecode — gives clients a
/// typed decoder straight out of the compiled artifact instead of a hand-written one
/// that can drift from the contract.
interface IAssetLens {
    function query(Request calldata req) external view returns (Result memory res);
}

/// @title AssetLens
/// @notice Reads an account's asset state — balances, allowances, NFT ids, account
///         code and delegation — in a single call, without being deployed.
///
/// ## The deployless trick
///
/// A constructor's return value becomes the deployed code. `eth_call` with no `to`
/// address runs creation code and hands back that "code", so a contract whose
/// constructor `return`s ABI-encoded data is a pure read function that never touches
/// the chain:
///
///     eth_call({ data: creationCode || abi.encode(request) }, "latest")
///
/// No deployment, no address to trust, no governance over an upgrade key, and no
/// per-chain deployment matrix — the lens works on any chain the moment it compiles.
/// The caller ships the bytecode with the question.
///
/// ## What that costs
///
/// Two consensus rules bound the trick, and both are the caller's to respect:
///
///   * EIP-170 — the returned "code" may be at most 24576 bytes, so a reply larger
///     than that fails the whole call. Batch, and shrink on failure.
///   * EIP-3860 — creation code plus arguments may be at most 49152 bytes.
///
/// internal/lens enforces both by chunking, which is why this contract is free to
/// stay linear and simple rather than trying to compress its own output.
///
/// ## Reading hostile contracts
///
/// Every external read is a bounded staticcall: capped gas, capped copied returndata,
/// and no revert that can take the batch down with it. A token that reverts, returns
/// garbage, returns a gigabyte or burns gas costs its own slot in the reply and
/// nothing else.
contract AssetLens {
    // ERC-20 / ERC-721 / ERC-1155 selectors.
    bytes4 private constant SEL_NAME = 0x06fdde03; // name()
    bytes4 private constant SEL_SYMBOL = 0x95d89b41; // symbol()
    bytes4 private constant SEL_DECIMALS = 0x313ce567; // decimals()
    bytes4 private constant SEL_TOTAL_SUPPLY = 0x18160ddd; // totalSupply()
    bytes4 private constant SEL_BALANCE_OF = 0x70a08231; // balanceOf(address)
    bytes4 private constant SEL_ALLOWANCE = 0xdd62ed3e; // allowance(address,address)
    bytes4 private constant SEL_SUPPORTS = 0x01ffc9a7; // supportsInterface(bytes4)
    bytes4 private constant SEL_APPROVED_ALL = 0xe985e9c5; // isApprovedForAll(address,address)
    bytes4 private constant SEL_OWNER_OF = 0x6352211e; // ownerOf(uint256)
    bytes4 private constant SEL_GET_APPROVED = 0x081812fc; // getApproved(uint256)
    bytes4 private constant SEL_TOKEN_URI = 0xc87b56dd; // tokenURI(uint256)
    bytes4 private constant SEL_URI = 0x0e89341c; // uri(uint256)
    bytes4 private constant SEL_BALANCE_1155 = 0x00fdd58e; // balanceOf(address,uint256)
    bytes4 private constant SEL_TOKEN_OF_OWNER = 0x2f745c59; // tokenOfOwnerByIndex(address,uint256)

    // ERC-165 interface ids.
    bytes4 private constant IID_ERC165 = 0x01ffc9a7;
    bytes4 private constant IID_INVALID = 0xffffffff;
    bytes4 private constant IID_ERC721 = 0x80ac58cd;
    bytes4 private constant IID_ERC721_ENUMERABLE = 0x780e9d63;
    bytes4 private constant IID_ERC1155 = 0xd9b67a26;

    uint8 private constant STANDARD_UNKNOWN = 0;
    uint8 private constant STANDARD_ERC20 = 20;
    uint8 private constant STANDARD_ERC721 = 21;
    uint8 private constant STANDARD_ERC1155 = 55;

    /// @dev Generous for an honest read (a cold SLOAD path is ~5k) and small enough
    ///      that a hostile token cannot starve the rest of the batch.
    uint256 private constant DEFAULT_CALL_GAS = 250_000;
    uint256 private constant DEFAULT_STRING_BYTES = 128;
    /// @dev Hard ceiling on any single string, whatever the request asks for: the
    ///      whole reply still has to fit in 24576 bytes.
    uint256 private constant MAX_STRING_BYTES = 1024;
    /// @dev Returndata beyond this is dropped before it is ever copied into memory,
    ///      so a returndatasize bomb cannot price the call out through memory growth.
    uint256 private constant MAX_RETURN_BYTES = 2048;
    /// @dev Enumeration is unbounded work on the token's side; keep it finite.
    uint256 private constant MAX_ENUMERATE = 256;

    /// @dev Request fields the per-token reads all need, normalised once and passed
    ///      as one reference. Internal only: it keeps the reading code out of the
    ///      stack-depth limit that a flat parameter list runs straight into.
    struct Ctx {
        address account;
        address[] spenders;
        bool includeUri;
        uint256 enumerate;
        uint256 gasPerCall;
        uint256 cap;
    }

    /// @notice Runs the query and returns `abi.encode(Result)` as the "deployed code".
    constructor(Request memory req) {
        bytes memory out = abi.encode(_run(req));
        assembly {
            return(add(out, 0x20), mload(out))
        }
    }

    // ----------------------------------------------------------------------
    // Query
    // ----------------------------------------------------------------------

    function _run(Request memory req) private view returns (Result memory res) {
        res.chain = ChainInfo({
            chainId: block.chainid,
            blockNumber: block.number,
            parentHash: block.number == 0 ? bytes32(0) : blockhash(block.number - 1),
            timestamp: block.timestamp,
            baseFee: block.basefee
        });
        res.account = _account(req.account, req.includeCode);

        uint256 cap = req.maxStringBytes == 0 ? DEFAULT_STRING_BYTES : req.maxStringBytes;
        Ctx memory c = Ctx({
            account: req.account,
            spenders: req.spenders,
            includeUri: req.includeUri,
            enumerate: req.enumerateLimit > MAX_ENUMERATE ? MAX_ENUMERATE : req.enumerateLimit,
            gasPerCall: req.gasPerCall == 0 ? DEFAULT_CALL_GAS : req.gasPerCall,
            cap: cap > MAX_STRING_BYTES ? MAX_STRING_BYTES : cap
        });

        res.tokens = new TokenInfo[](req.tokens.length);
        for (uint256 i = 0; i < req.tokens.length; i++) {
            res.tokens[i] = _token(req.tokens[i], c);
        }
    }

    /// @dev Account state readable from inside the EVM. Nonce is not, deliberately
    ///      absent rather than faked.
    function _account(address account, bool includeCode) private view returns (AccountInfo memory a) {
        a.account = account;
        a.balance = account.balance;
        a.codeSize = _codeSize(account);
        a.codeHash = account.codehash;

        // EIP-7702 delegation designator: 0xef0100 || 20-byte target, always 23 bytes.
        if (a.codeSize == 23) {
            bytes memory code = account.code;
            if (uint8(code[0]) == 0xEF && uint8(code[1]) == 0x01 && uint8(code[2]) == 0x00) {
                address d;
                assembly ("memory-safe") {
                    d := shr(96, mload(add(code, 35)))
                }
                a.isDelegated = true;
                a.delegate = d;
            }
        }
        a.isContract = a.codeSize > 0 && !a.isDelegated;
        if (includeCode) a.code = account.code;
    }

    function _token(TokenQuery memory q, Ctx memory c) private view returns (TokenInfo memory t) {
        t.token = q.token;
        t.isContract = _codeSize(q.token) > 0;
        t.allowances = new uint256[](c.spenders.length);
        t.allowanceKnown = new bool[](c.spenders.length);
        t.approvedForAll = new bool[](c.spenders.length);
        t.ids = new TokenIdInfo[](0);
        if (!t.isContract) return t;

        _detect(t, c);
        _metadata(t, c);
        t.standard = _classify(t);
        _approvals(t, c);
        _resolveIds(t, q.ids, c);
    }

    /// @dev ERC-165 first: it is the only self-description that is actually binding,
    ///      and it decides which of the later reads are meaningful at all. A contract
    ///      that answers `true` to the reserved invalid id is answering everything
    ///      `true`, so its claims are worth nothing.
    function _detect(TokenInfo memory t, Ctx memory c) private view {
        t.supportsErc165 =
            _supports(t.token, IID_ERC165, c.gasPerCall) && !_supports(t.token, IID_INVALID, c.gasPerCall);
        if (!t.supportsErc165) return;
        t.isErc721 = _supports(t.token, IID_ERC721, c.gasPerCall);
        t.isErc1155 = _supports(t.token, IID_ERC1155, c.gasPerCall);
        if (t.isErc721) t.isEnumerable = _supports(t.token, IID_ERC721_ENUMERABLE, c.gasPerCall);
    }

    function _metadata(TokenInfo memory t, Ctx memory c) private view {
        (bool okSym, bytes memory rSym) = _call(t.token, abi.encodeWithSelector(SEL_SYMBOL), c.gasPerCall);
        if (okSym) (t.symbol,) = _string(rSym, c.cap);

        (bool okName, bytes memory rName) = _call(t.token, abi.encodeWithSelector(SEL_NAME), c.gasPerCall);
        if (okName) (t.name,) = _string(rName, c.cap);

        (bool okDec, bytes memory rDec) = _call(t.token, abi.encodeWithSelector(SEL_DECIMALS), c.gasPerCall);
        if (okDec) {
            (uint256 d, bool ok) = _uint(rDec);
            // A token claiming more than 77 decimals is lying or broken: 10**78
            // overflows a uint256, so no honest unit scale lives up there.
            if (ok && d <= 77) {
                t.decimals = uint8(d);
                t.hasDecimals = true;
            }
        }

        (bool okSupply, bytes memory rSupply) = _call(t.token, abi.encodeWithSelector(SEL_TOTAL_SUPPLY), c.gasPerCall);
        if (okSupply) (t.totalSupply, t.hasTotalSupply) = _uint(rSupply);

        // ERC-1155 has no balanceOf(address), so asking is pointless there.
        if (!t.isErc1155) {
            (bool okBal, bytes memory rBal) =
                _call(t.token, abi.encodeWithSelector(SEL_BALANCE_OF, c.account), c.gasPerCall);
            if (okBal) (t.balance, t.hasBalance) = _uint(rBal);
        }
    }

    /// @dev Standard from evidence, in order of how binding that evidence is: ERC-165
    ///      answers first, then the ERC-20 shape. Anything else stays unknown rather
    ///      than being guessed into a category a wallet would render wrongly.
    function _classify(TokenInfo memory t) private pure returns (uint8) {
        if (t.isErc721) return STANDARD_ERC721;
        if (t.isErc1155) return STANDARD_ERC1155;
        if (t.hasDecimals && (t.hasBalance || t.hasTotalSupply)) return STANDARD_ERC20;
        return STANDARD_UNKNOWN;
    }

    /// @dev Allowance for fungibles, operator approval for NFTs. They answer the same
    ///      wallet question — "what can this spender move on my behalf" — so they
    ///      share a slot per spender rather than forcing the caller to ask twice.
    function _approvals(TokenInfo memory t, Ctx memory c) private view {
        for (uint256 i = 0; i < c.spenders.length; i++) {
            if (t.standard == STANDARD_ERC721 || t.standard == STANDARD_ERC1155) {
                (bool ok, bytes memory r) = _call(
                    t.token, abi.encodeWithSelector(SEL_APPROVED_ALL, c.account, c.spenders[i]), c.gasPerCall
                );
                if (ok) t.approvedForAll[i] = _bool(r);
            } else {
                (bool ok, bytes memory r) =
                    _call(t.token, abi.encodeWithSelector(SEL_ALLOWANCE, c.account, c.spenders[i]), c.gasPerCall);
                if (ok) (t.allowances[i], t.allowanceKnown[i]) = _uint(r);
            }
        }
    }

    function _resolveIds(TokenInfo memory t, uint256[] memory requested, Ctx memory c) private view {
        uint256[] memory ids = requested;
        if (ids.length == 0 && c.enumerate > 0 && t.isEnumerable && t.hasBalance) {
            ids = _enumerate(t.token, c.account, t.balance < c.enumerate ? t.balance : c.enumerate, c.gasPerCall);
        }
        t.ids = new TokenIdInfo[](ids.length);
        for (uint256 i = 0; i < ids.length; i++) {
            t.ids[i] = _tokenId(t.token, ids[i], t.standard, c);
        }
    }

    /// @dev ERC-721Enumerable turns "which NFTs does this account hold" into an
    ///      answerable question without an index behind it. Stops at the first
    ///      failure: the enumeration is only meaningful while it is contiguous.
    function _enumerate(address token, address account, uint256 n, uint256 gasPerCall)
        private
        view
        returns (uint256[] memory ids)
    {
        uint256[] memory buf = new uint256[](n);
        uint256 found;
        for (uint256 i = 0; i < n; i++) {
            (bool ok, bytes memory r) = _call(token, abi.encodeWithSelector(SEL_TOKEN_OF_OWNER, account, i), gasPerCall);
            if (!ok) break;
            (uint256 id, bool okId) = _uint(r);
            if (!okId) break;
            buf[found++] = id;
        }
        ids = new uint256[](found);
        for (uint256 i = 0; i < found; i++) {
            ids[i] = buf[i];
        }
    }

    function _tokenId(address token, uint256 id, uint8 standard, Ctx memory c)
        private
        view
        returns (TokenIdInfo memory info)
    {
        info.id = id;

        if (standard != STANDARD_ERC1155) {
            (bool ok, bytes memory r) = _call(token, abi.encodeWithSelector(SEL_OWNER_OF, id), c.gasPerCall);
            if (ok) {
                (info.owner, info.ownerKnown) = _address(r);
                if (info.ownerKnown) {
                    info.balance = info.owner == c.account ? 1 : 0;
                    info.balanceKnown = true;
                }
            }
            (bool okA, bytes memory rA) = _call(token, abi.encodeWithSelector(SEL_GET_APPROVED, id), c.gasPerCall);
            if (okA) (info.approved, info.approvedKnown) = _address(rA);
        }

        if (standard != STANDARD_ERC721) {
            (bool ok, bytes memory r) =
                _call(token, abi.encodeWithSelector(SEL_BALANCE_1155, c.account, id), c.gasPerCall);
            // ownerOf already settled this for a 721 that lacks ERC-165 but answers
            // both shapes; the 721 answer is the more reliable one, so it wins.
            if (ok && !info.balanceKnown) (info.balance, info.balanceKnown) = _uint(r);
        }

        if (c.includeUri) {
            bytes4 sel = standard == STANDARD_ERC1155 ? SEL_URI : SEL_TOKEN_URI;
            (bool ok, bytes memory r) = _call(token, abi.encodeWithSelector(sel, id), c.gasPerCall);
            if (ok) (info.uri, info.uriTruncated) = _string(r, c.cap);
        }
    }

    // ----------------------------------------------------------------------
    // Bounded calls and tolerant decoding
    // ----------------------------------------------------------------------

    /// @dev EXTCODESIZE, not `addr.code.length`: the latter is free to copy the code
    ///      into memory to measure it, and paying 24KB of memory per token to learn a
    ///      single number is not a trade worth making inside a batch.
    function _codeSize(address a) private view returns (uint256 size) {
        assembly ("memory-safe") {
            size := extcodesize(a)
        }
    }

    function _supports(address token, bytes4 iid, uint256 gasPerCall) private view returns (bool) {
        (bool ok, bytes memory r) = _call(token, abi.encodeWithSelector(SEL_SUPPORTS, iid), gasPerCall);
        return ok && _bool(r);
    }

    /// @dev staticcall with capped gas and capped returndata copy. Never reverts:
    ///      the callee's failure is data, not a failure of this call.
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

    function _uint(bytes memory ret) private pure returns (uint256 v, bool ok) {
        if (ret.length < 32) return (0, false);
        assembly {
            v := mload(add(ret, 0x20))
        }
        ok = true;
    }

    function _bool(bytes memory ret) private pure returns (bool) {
        (uint256 v, bool ok) = _uint(ret);
        return ok && v != 0;
    }

    /// @dev Masks the low 20 bytes: plenty of live contracts return an address word
    ///      with dirty high bits, and rejecting those helps nobody.
    function _address(bytes memory ret) private pure returns (address a, bool ok) {
        (uint256 v, bool okv) = _uint(ret);
        return (address(uint160(v)), okv);
    }

    /// @dev Decodes both the ABI string form and the bytes32 form that predates it
    ///      (MKR and other early tokens return bytes32 from name()/symbol()), and
    ///      truncates to `cap`. Reports whether it truncated so a caller can tell a
    ///      short name from a clipped one.
    function _string(bytes memory ret, uint256 cap) private pure returns (string memory s, bool truncated) {
        if (ret.length == 32) {
            uint256 n;
            while (n < 32 && ret[n] != 0) {
                n++;
            }
            if (n > cap) {
                n = cap;
                truncated = true;
            }
            bytes memory b = new bytes(n);
            for (uint256 i = 0; i < n; i++) {
                b[i] = ret[i];
            }
            return (string(b), truncated);
        }
        if (ret.length < 64) return ("", false);

        (uint256 off,) = _uint(ret);
        // Bounds are checked against what we actually copied, which may already be a
        // truncation of a hostile reply: a claimed offset is never trusted. Note the
        // subtraction rather than `off + 32`: checked arithmetic turns an overflowing
        // offset into a revert, and one hostile token must not take the batch down.
        if (off > ret.length || ret.length - off < 32) return ("", false);
        uint256 len;
        assembly {
            len := mload(add(add(ret, 0x20), off))
        }
        uint256 avail = ret.length - off - 32;
        if (len > avail) {
            len = avail;
            truncated = true;
        }
        if (len > cap) {
            len = cap;
            truncated = true;
        }
        bytes memory out = new bytes(len);
        for (uint256 i = 0; i < len; i++) {
            out[i] = ret[off + 32 + i];
        }
        return (string(out), truncated);
    }
}
