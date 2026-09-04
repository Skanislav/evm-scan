// Package evmlog decodes the small set of token events that carry account identity.
//
// Discovery only needs to answer "which accounts touched this contract", so we decode
// participants out of indexed topics and ignore event data entirely. That keeps the
// decoder total (no ABI unpacking, no failure modes on odd tokens) and lets the node
// pre-filter by topic0, which is what keeps the scan cheap.
package evmlog

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Standard is the token interface a log appears to belong to.
type Standard uint8

const (
	StandardUnknown Standard = 0
	StandardERC20   Standard = 20
	StandardERC721  Standard = 21
	StandardERC1155 Standard = 55
)

func (s Standard) String() string {
	switch s {
	case StandardERC20:
		return "erc20"
	case StandardERC721:
		return "erc721"
	case StandardERC1155:
		return "erc1155"
	default:
		return "unknown"
	}
}

// Event signature hashes (topic0).
var (
	SigTransfer       = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	SigApproval       = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
	SigApprovalForAll = crypto.Keccak256Hash([]byte("ApprovalForAll(address,address,bool)"))
	SigTransferSingle = crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))
	SigTransferBatch  = crypto.Keccak256Hash([]byte("TransferBatch(address,address,address,uint256[],uint256[])"))
)

// Role records why an account appears in an event. It is kept alongside the
// interaction because "received a token" and "approved a spender" are very different
// signals to a wallet consuming these hints.
type Role uint8

const (
	RoleSender Role = iota + 1
	RoleReceiver
	RoleOwner
	RoleSpender
	RoleOperator
)

func (r Role) String() string {
	switch r {
	case RoleSender:
		return "sender"
	case RoleReceiver:
		return "receiver"
	case RoleOwner:
		return "owner"
	case RoleSpender:
		return "spender"
	case RoleOperator:
		return "operator"
	default:
		return "unknown"
	}
}

// Participant is one account extracted from a log.
type Participant struct {
	Address common.Address
	Role    Role
}

// Decoded is the discovery-relevant projection of a log.
type Decoded struct {
	Standard     Standard
	Event        string
	Participants []Participant
}

// WatchedTopics is the topic0 filter set. Passing this to eth_getLogs and
// eth_subscribe means the node never ships us logs we would discard.
func WatchedTopics() [][]common.Hash {
	return [][]common.Hash{{
		SigTransfer,
		SigApproval,
		SigApprovalForAll,
		SigTransferSingle,
		SigTransferBatch,
	}}
}

// Decode extracts participants from a log. ok is false for anything we do not treat
// as an identity-carrying token event.
//
// ERC-20 and ERC-721 share the Transfer and Approval signatures and are told apart by
// topic count: ERC-721 indexes the tokenId, so it carries one extra topic.
func Decode(l *types.Log) (Decoded, bool) {
	if len(l.Topics) == 0 {
		return Decoded{}, false
	}

	switch l.Topics[0] {
	case SigTransfer:
		switch len(l.Topics) {
		case 3:
			return pair(StandardERC20, "Transfer", l, RoleSender, RoleReceiver)
		case 4:
			return pair(StandardERC721, "Transfer", l, RoleSender, RoleReceiver)
		}

	case SigApproval:
		switch len(l.Topics) {
		case 3:
			return pair(StandardERC20, "Approval", l, RoleOwner, RoleSpender)
		case 4:
			return pair(StandardERC721, "Approval", l, RoleOwner, RoleSpender)
		}

	case SigApprovalForAll:
		if len(l.Topics) == 3 {
			// Shared by ERC-721 and ERC-1155; the standard is settled by the
			// contract's other events, so stay uncommitted here.
			return pair(StandardUnknown, "ApprovalForAll", l, RoleOwner, RoleOperator)
		}

	case SigTransferSingle, SigTransferBatch:
		if len(l.Topics) == 4 {
			name := "TransferSingle"
			if l.Topics[0] == SigTransferBatch {
				name = "TransferBatch"
			}
			return triple(StandardERC1155, name, l, RoleOperator, RoleSender, RoleReceiver)
		}
	}

	return Decoded{}, false
}

func pair(std Standard, event string, l *types.Log, r1, r2 Role) (Decoded, bool) {
	d := Decoded{Standard: std, Event: event}
	d.append(l.Topics[1], r1)
	d.append(l.Topics[2], r2)
	if len(d.Participants) == 0 {
		return Decoded{}, false
	}
	return d, true
}

func triple(std Standard, event string, l *types.Log, r1, r2, r3 Role) (Decoded, bool) {
	d := Decoded{Standard: std, Event: event}
	d.append(l.Topics[1], r1)
	d.append(l.Topics[2], r2)
	d.append(l.Topics[3], r3)
	if len(d.Participants) == 0 {
		return Decoded{}, false
	}
	return d, true
}

func (d *Decoded) append(topic common.Hash, role Role) {
	addr, ok := topicAddress(topic)
	if !ok {
		return
	}
	for _, p := range d.Participants {
		if p.Address == addr && p.Role == role {
			return
		}
	}
	d.Participants = append(d.Participants, Participant{Address: addr, Role: role})
}

// topicAddress reads an address out of a 32-byte topic.
//
// It rejects topics with dirty high bytes: an unrelated event can collide on topic0
// while indexing a non-address parameter, and admitting those would pollute the index
// with addresses that never existed. The zero address is dropped too, since mint and
// burn counterparties are not accounts anyone wants discovered.
func topicAddress(t common.Hash) (common.Address, bool) {
	for _, b := range t[:12] {
		if b != 0 {
			return common.Address{}, false
		}
	}
	addr := common.BytesToAddress(t[12:])
	if addr == (common.Address{}) {
		return common.Address{}, false
	}
	return addr, true
}

// InferStandard folds the standards observed across a contract's logs into a single
// classification. ERC-1155 and ERC-721 evidence beats the ERC-20 default because the
// ERC-20 shape is the one produced by an ambiguous two-topic Transfer.
func InferStandard(seen map[Standard]int) Standard {
	switch {
	case seen[StandardERC1155] > 0:
		return StandardERC1155
	case seen[StandardERC721] > 0:
		return StandardERC721
	case seen[StandardERC20] > 0:
		return StandardERC20
	default:
		return StandardUnknown
	}
}
