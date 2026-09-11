package api

import (
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/ens"
)

// hintName is the ENS name HintResolver serves this account's index under, or ""
// when the deployment has no resolver attached (registry.ens_parent unset). The
// chain id becomes a label only when it is not the deployment's first chain, which
// is the resolver's default; see docs/ENS.md.
//
// This is a formatter and nothing more. The daemon does not resolve names in either
// direction: a reader's page or mirror does that against its own RPC.
func (s *Server) hintName(account common.Address, chainID uint64) string {
	if s.d.ENSParent == "" {
		return ""
	}
	explicit := true
	if first, ok := s.d.Chains.First(); ok && first.ID == chainID {
		explicit = false
	}
	return ens.HintName(account, s.d.ENSParent, chainID, explicit)
}
