// Package chainprofile holds the per-chain tuning that cannot be discovered.
//
// This is the same posture as internal/price/defaults.go, applied to the rest of
// the parameters: a table of what is known about the chains people actually index,
// so that pointing the daemon at one does not require the operator to also know its
// block time. Everything here is a default. Config overrides it, and a chain that
// is not in the table still works — it just gets conservative numbers and says so.
//
// It exists because these values are not optional and not derivable. ENS's chain
// registry answers "what chain id is base" and carries nothing operational (see
// internal/ens); a node answers "what is the head" but not "how deep is a reorg
// here". And the numbers are not close to each other: mainnet produces a block
// every 12 seconds and Arbitrum roughly every quarter of one, so a confirmation
// depth that means two minutes on one chain means three seconds on the other.
// Inheriting the wrong one is not a small error.
package chainprofile

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Profile is what we know about a chain before dialling it.
type Profile struct {
	// Name is the short name the daemon logs and the UI shows.
	Name string
	// Testnet marks a chain whose data is disposable. Nothing enforces this; it is
	// worth showing so an operator does not confuse two chains with one name.
	Testnet bool

	// NativeSymbol and NativeDecimals name the gas asset. The portfolio endpoint
	// reports a native balance, and without these it reports it unlabelled — which
	// is merely unhelpful on Ethereum and actively wrong anywhere the native asset
	// is not ETH.
	NativeSymbol   string
	NativeDecimals int16
	// WrappedNative is the ERC-20 wrapper, which pricing needs as its bridge from
	// a token quote to a native one.
	WrappedNative common.Address

	// BlockTime is the target interval between blocks. Everything below is derived
	// from it, and it is recorded so the derivation can be checked.
	BlockTime time.Duration
	// Confirmations is the depth at which events are folded into the rollup. Chosen
	// as roughly one to two minutes of wall clock, because that is the unit reorg
	// risk is actually measured in — not a block count that happens to be familiar.
	Confirmations uint64
	// BackfillWindow and TailWindow bound one eth_getLogs. SweepLogs halves these
	// when a provider complains, so being slightly high is self-correcting and
	// being very high is not.
	BackfillWindow uint64
	TailWindow     uint64
	// PollInterval is how often the follower ticks when nothing wakes it.
	PollInterval time.Duration
	// BackfillInterval paces the backward walk.
	BackfillInterval time.Duration
}

// Generic is the profile for a chain nobody has written down.
//
// Deliberately timid. A runtime-added chain is usually a third-party RPC, and
// third-party RPCs cap eth_getLogs in ways that are not uniform and not always
// reported as a range error: a 10 MB response cap and a 10-block range cap both
// exist in the wild and only one of them is something SweepLogs can back off from.
// Slow and correct recovers; fast and refused does not. An operator who knows
// better overrides it, and the API says the numbers are guesses.
func Generic() Profile {
	return Profile{
		Name:             "unknown",
		NativeSymbol:     "ETH",
		NativeDecimals:   18,
		BlockTime:        2 * time.Second,
		Confirmations:    30,
		BackfillWindow:   100,
		TailWindow:       50,
		PollInterval:     4 * time.Second,
		BackfillInterval: 2 * time.Second,
	}
}

var addr = common.HexToAddress

// known is the table. Add a chain here rather than teaching callers about it.
var known = map[uint64]Profile{
	1: {
		Name: "mainnet", NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative:  addr("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		BlockTime:      12 * time.Second,
		Confirmations:  12, // ~2.5 min, and the one chain where reorgs are routine
		BackfillWindow: 5000, TailWindow: 1000,
		PollInterval: 6 * time.Second, BackfillInterval: time.Second,
	},
	10: {
		Name: "optimism", NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative:  addr("0x4200000000000000000000000000000000000006"),
		BlockTime:      2 * time.Second,
		Confirmations:  30, // ~1 min
		BackfillWindow: 2000, TailWindow: 500,
		PollInterval: 2 * time.Second, BackfillInterval: time.Second,
	},
	8453: {
		Name: "base", NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative:  addr("0x4200000000000000000000000000000000000006"),
		BlockTime:      2 * time.Second,
		Confirmations:  30, // ~1 min
		BackfillWindow: 2000, TailWindow: 500,
		PollInterval: 2 * time.Second, BackfillInterval: time.Second,
	},
	42161: {
		Name: "arbitrum", NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative: addr("0x82aF49447D8a07e3bd95BD0d56f35241523fBaB1"),
		// Arbitrum blocks land about four times a second, which is why the
		// confirmation depth here looks alarming next to mainnet's and is not.
		BlockTime:      250 * time.Millisecond,
		Confirmations:  240, // ~1 min
		BackfillWindow: 5000, TailWindow: 2000,
		PollInterval: time.Second, BackfillInterval: time.Second,
	},

	// Testnets. Same shapes, smaller windows: these are usually reached over a
	// public endpoint rather than a node of one's own.
	11155111: {
		Name: "sepolia", Testnet: true, NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative:  addr("0xfFf9976782d46CC05630D1f6eBAb18b2324d6B14"),
		BlockTime:      12 * time.Second,
		Confirmations:  6,
		BackfillWindow: 500, TailWindow: 200,
		PollInterval: 12 * time.Second, BackfillInterval: 2 * time.Second,
	},
	84532: {
		Name: "base-sepolia", Testnet: true, NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative:  addr("0x4200000000000000000000000000000000000006"),
		BlockTime:      2 * time.Second,
		Confirmations:  15,
		BackfillWindow: 500, TailWindow: 200,
		PollInterval: 4 * time.Second, BackfillInterval: 2 * time.Second,
	},
	11155420: {
		Name: "optimism-sepolia", Testnet: true, NativeSymbol: "ETH", NativeDecimals: 18,
		WrappedNative:  addr("0x4200000000000000000000000000000000000006"),
		BlockTime:      2 * time.Second,
		Confirmations:  15,
		BackfillWindow: 500, TailWindow: 200,
		PollInterval: 4 * time.Second, BackfillInterval: 2 * time.Second,
	},

	// `geth --dev`, which mines on demand: there is no block time to speak of and
	// nothing to reorg, so confirm immediately or the demo indexes nothing.
	1337: devProfile("dev"),
	// Hardhat and Anvil's default.
	31337: devProfile("dev"),
}

func devProfile(name string) Profile {
	return Profile{
		Name: name, Testnet: true, NativeSymbol: "ETH", NativeDecimals: 18,
		BlockTime:      time.Second,
		Confirmations:  0,
		BackfillWindow: 5000, TailWindow: 5000,
		PollInterval: time.Second, BackfillInterval: 200 * time.Millisecond,
	}
}

// Lookup returns the built-in profile for a chain, and whether there is one.
//
// A false second return is not a failure — it means the caller should use Generic
// and tell the operator the numbers are guesses, which is a different thing from
// refusing to index the chain.
func Lookup(chainID uint64) (Profile, bool) {
	p, ok := known[chainID]
	return p, ok
}

// For returns a usable profile for any chain: the built-in one where it exists,
// Generic otherwise, with the name filled in from the caller when we have none.
func For(chainID uint64, fallbackName string) Profile {
	if p, ok := known[chainID]; ok {
		return p
	}
	p := Generic()
	if fallbackName != "" {
		p.Name = fallbackName
	}
	return p
}

// Known lists the chain ids in the table, for /v1/status and the UI.
func Known() []uint64 {
	out := make([]uint64, 0, len(known))
	for id := range known {
		out = append(out, id)
	}
	return out
}
