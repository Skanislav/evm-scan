// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Minimal ERC-721 for the demo chain.
/// @dev Its Transfer event carries an indexed tokenId, giving the four-topic shape the
///      log decoder uses to tell ERC-721 apart from ERC-20.
contract DemoERC721 {
    string public name;
    string public symbol;
    uint256 public nextTokenId;

    mapping(uint256 => address) public ownerOf;
    mapping(address => uint256) public balanceOf;
    mapping(uint256 => address) public getApproved;
    mapping(address => mapping(address => bool)) public isApprovedForAll;

    event Transfer(address indexed from, address indexed to, uint256 indexed tokenId);
    event Approval(address indexed owner, address indexed approved, uint256 indexed tokenId);
    event ApprovalForAll(address indexed owner, address indexed operator, bool approved);

    error NotOwner();
    error NotAuthorised();

    constructor(string memory name_, string memory symbol_) {
        name = name_;
        symbol = symbol_;
    }

    /// @notice ERC-165, claiming ERC-165 itself and ERC-721.
    /// @dev Here because it is how a reader is *supposed* to tell an NFT apart from a
    ///      fungible token: without it, a lens or an indexer is left guessing from the
    ///      shape of the interface. Deliberately does not claim ERC-721Metadata, which
    ///      this demo does not implement.
    function supportsInterface(bytes4 interfaceId) external pure returns (bool) {
        return interfaceId == 0x01ffc9a7 || interfaceId == 0x80ac58cd;
    }

    function mint(address to) external returns (uint256 tokenId) {
        tokenId = nextTokenId++;
        ownerOf[tokenId] = to;
        balanceOf[to] += 1;
        emit Transfer(address(0), to, tokenId);
    }

    function approve(address to, uint256 tokenId) external {
        address owner = ownerOf[tokenId];
        if (owner != msg.sender && !isApprovedForAll[owner][msg.sender]) revert NotAuthorised();
        getApproved[tokenId] = to;
        emit Approval(owner, to, tokenId);
    }

    function setApprovalForAll(address operator, bool approved) external {
        isApprovedForAll[msg.sender][operator] = approved;
        emit ApprovalForAll(msg.sender, operator, approved);
    }

    function transferFrom(address from, address to, uint256 tokenId) public {
        if (ownerOf[tokenId] != from) revert NotOwner();
        if (msg.sender != from && getApproved[tokenId] != msg.sender && !isApprovedForAll[from][msg.sender]) {
            revert NotAuthorised();
        }
        delete getApproved[tokenId];
        ownerOf[tokenId] = to;
        unchecked {
            balanceOf[from] -= 1;
            balanceOf[to] += 1;
        }
        emit Transfer(from, to, tokenId);
    }
}
