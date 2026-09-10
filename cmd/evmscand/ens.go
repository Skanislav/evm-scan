package main

import (
	"context"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/chainset"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/ens"
)

// mainnetChainID is where the on.eth chain registry lives.
const mainnetChainID = 1

// newENSResolver picks the node that will answer "what chain is `base`".
//
// Preference order, and the reason for it:
//
//  1. A mainnet chain this deployment already indexes. Resolution is then one
//     eth_call at head on a node we already run, and costs nothing extra. This is
//     the common case for the topology this is aimed at — mainnet indexed, the
//     registry on Base, and other chains alongside.
//  2. ens.node from the config. A Sepolia-only or Helios deployment has no
//     mainnet node of its own, and pointing this at a public endpoint is a
//     reasonable thing to do: it is a read at head, once per onboarding, and it
//     is checked against the dialled node's own chain id before anything is
//     written. It is the one place this design knowingly reads from somebody
//     else's node, and it buys nothing that could be forged into an index.
//
// With neither, name resolution is simply unavailable and the UI asks for a
// chain id by hand — which is the path most chains take anyway, since most are
// not in the registry.
func newENSResolver(ctx context.Context, set *chainset.Set, cfg *config.Config, log *slog.Logger) *ens.Resolver {
	if !cfg.ENS.Enabled() {
		log.Info("chain name resolution off by config")
		return nil
	}

	resolverAddr := common.Address{}
	if cfg.ENS.Resolver != "" {
		resolverAddr = common.HexToAddress(cfg.ENS.Resolver)
	}

	if src, ok := set.Source(mainnetChainID); ok {
		r := ens.New(src, resolverAddr)
		log.Info("chain names resolve through the indexed mainnet node",
			"registry", ens.Parent, "resolver", r.Address().Hex())
		return r
	}

	if cfg.ENS.Node == "" {
		log.Info("chain name resolution unavailable: no Ethereum mainnet endpoint",
			"note", "index mainnet, or set ens.node; chains can still be added by chain id")
		return nil
	}

	// Read-only, at head, and not part of any index. require_local_node does not
	// apply: nothing this node says can end up in a commitment, because the chain
	// id it returns is cross-checked against the node actually being indexed.
	src, err := chain.Dial(ctx, cfg.ENS.Node, false)
	if err != nil {
		log.Warn("could not dial ens.node; chain name resolution unavailable", "err", err)
		return nil
	}
	id, err := src.ChainID(ctx)
	if err != nil || id != mainnetChainID {
		// A resolver pointed at the wrong network would answer confidently and
		// wrongly, which is worse than not answering.
		src.Close()
		log.Warn("ens.node is not Ethereum mainnet; chain name resolution unavailable",
			"chain_id", id, "err", err)
		return nil
	}
	r := ens.New(src, resolverAddr)
	log.Info("chain names resolve through ens.node",
		"registry", ens.Parent, "resolver", r.Address().Hex(),
		"node", src.Endpoint().Redacted())
	return r
}
