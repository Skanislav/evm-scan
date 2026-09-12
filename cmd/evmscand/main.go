// Command evmscand runs the indexer, the registry mirror and the HTTP API.
//
// It is deliberately one process: at the scale this design targets, the index is
// bounded by the registered asset set rather than by chain size, so splitting the
// workers from the API would add operational surface without buying anything.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/api"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/chainset"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/store"
)

func main() {
	var (
		cfgPath  = flag.String("config", "config.yaml", "path to the YAML config file")
		webDir   = flag.String("web", "web", "directory of static demo UI files (empty to disable)")
		logLevel = flag.String("log-level", "info", "debug, info, warn or error")
	)
	flag.Parse()

	log := newLogger(*logLevel)

	if err := run(*cfgPath, *webDir, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func run(cfgPath, webDir string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer st.Close()

	if cfg.Database.AutoMigrate {
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		log.Info("database schema up to date")
	}

	// One set rather than four parallel maps. Config order is its insertion order,
	// which is what "the chain this deployment is mainly about" means to the API,
	// and it is guarded because it stops being immutable as soon as a chain can be
	// added over HTTP.
	set := chainset.New()
	defer set.CloseAll()

	var wg sync.WaitGroup
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sv := &supervisor{st: st, set: set, log: log, runCtx: runCtx, cancel: cancel, wg: &wg}
	// Token lists come from config, so they are loaded once here alongside the
	// chains that config names. A chain added over HTTP later has no configured
	// list and simply gets no token filter, which is the same answer as a
	// configured chain that named none.
	tokenFilters := map[uint64]*hintfilter.Cache{}
	tokenAddrs := map[uint64][]common.Address{}

	for _, c := range cfg.Chains {
		if err := sv.startConfigChain(ctx, c); err != nil {
			return err
		}
		e, _ := set.Get(c.ChainID)
		log.Info("chain ready",
			"chain_id", c.ChainID, "name", c.Name,
			"node", e.Source.Endpoint().String(), "confirmations", c.Confirmations,
			"discovery", c.Discovery.Enabled, "auto_promote", c.Discovery.AutoPromote)

		// Prices come off the same node, through the same deployless trick, from
		// whatever oracles and pools the chain has. Where they come from is worth
		// a log line: it is the only third-party code in the read path.
		if p := e.Pricer; p != nil {
			src := p.Sources()
			log.Info("price discovery enabled",
				"chain_id", c.ChainID,
				"feed_registry", src.FeedRegistry != (common.Address{}),
				"native_usd_feed", src.NativeUSDFeed != (common.Address{}),
				"pinned_feeds", len(src.Feeds),
				"uniswap_v3", src.V3Factory != (common.Address{}),
				"uniswap_v2", src.V2Factory != (common.Address{}),
				"quote_tokens", len(src.QuoteTokens),
				"twap_window", p.TWAPWindow())
		} else {
			log.Info("price discovery off: no on-chain sources configured for this chain", "chain_id", c.ChainID)
		}

		// Token lists are fetched once, here, rather than on a request path. They
		// are the one third-party HTTP dependency in this process, and a failure to
		// reach one is logged and shrugged off: an optional hint should never be
		// the reason a deployment will not start.
		tf, addrs, err := api.LoadTokenLists(ctx, c.ChainID, c.TokenLists, log)
		if err != nil {
			log.Warn("no token filter for this chain", "chain_id", c.ChainID, "err", err)
		} else if tf != nil {
			tokenFilters[c.ChainID] = tf
			tokenAddrs[c.ChainID] = addrs
		}
	}

	// Chains somebody added over the API in an earlier run. They come up after
	// the config ones, so the configured first chain stays the one a request
	// without a chain_id means. A failure here is not fatal — the operator can
	// fix the endpoint through the same API that added it.
	if stored, err := st.ListChainProfiles(ctx, true); err == nil {
		for _, p := range stored {
			if p.Source == store.ChainSourceConfig {
				continue
			}
			if _, running := set.Get(p.ChainID); running {
				continue
			}
			if err := sv.StartStored(ctx, p); err != nil {
				log.Warn("could not restart a chain added earlier",
					"chain_id", p.ChainID, "name", p.Name, "err", err)
			}
		}
	}

	// Chain names resolve through ENS's on.eth registry, which lives on Ethereum
	// mainnet. A deployment that indexes mainnet already has the node for it and
	// pays nothing extra; one that does not can point ens.node at any mainnet
	// endpoint, since this is a read at head and nothing else.
	ensResolver := newENSResolver(ctx, set, cfg, log)

	var (
		regClient *hintreg.Client
		publisher *hintreg.Publisher
		relay     hintreg.Submitter
		mirror    *hintreg.Mirror
	)

	if addr, ok := cfg.RegistryAddress(); ok {
		regSrc, ok := set.Source(cfg.Registry.ChainID)
		if !ok {
			return fmt.Errorf("registry chain %d is not among the configured chains", cfg.Registry.ChainID)
		}
		regClient, err = hintreg.NewClient(regSrc, addr)
		if err != nil {
			return err
		}

		// All three read through the set rather than closing over a snapshot of
		// it, so a chain added while the mirror is running is mirrored at once.
		head := func(ctx context.Context, chainID uint64) (uint64, bool, error) {
			src, ok := set.Source(chainID)
			if !ok {
				return 0, false, nil
			}
			h, err := src.HeadBlock(ctx)
			return h, true, err
		}

		code := func(ctx context.Context, chainID uint64, addr common.Address) ([]byte, error) {
			src, ok := set.Source(chainID)
			if !ok {
				return nil, fmt.Errorf("no source for chain %d", chainID)
			}
			return src.CodeAt(ctx, addr)
		}

		nudge := func(chainID uint64) (hintreg.Nudger, bool) {
			return set.Worker(chainID)
		}
		mirror = hintreg.NewMirror(regClient, st, cfg.Registry.ChainID, head, code, nudge, log)

		// How this registry settles disputes is fixed at its deployment and is not
		// something an operator can change, so it belongs in the startup log where it
		// can be read off a running deployment.
		adjudication := "unknown"
		if mode, err := regClient.Mode(ctx); err != nil {
			// Not fatal: the mirror retries, and a registry we cannot read yet is a
			// node problem rather than a misconfiguration.
			log.Warn("could not read registry adjudication mode", "err", err)
		} else {
			adjudication = mode.String()
		}
		log.Info("hint registry mirrored",
			"address", addr.Hex(), "chain_id", cfg.Registry.ChainID, "adjudication", adjudication)

		if cfg.Registry.PublisherKey != "" {
			sub, err := newSubmitter(regSrc, cfg.Registry)
			if err != nil {
				return err
			}
			relay = sub
			publisher = hintreg.NewPublisher(regClient, sub, st, log)
			publisher.StaleAfter = cfg.Registry.Publisher.StaleAfter.D()
			if raw := cfg.Registry.Publisher.MinExpectedReward; raw != "" {
				minReward, ok := new(big.Int).SetString(raw, 10)
				if !ok || minReward.Sign() < 0 {
					return fmt.Errorf("registry.publisher.min_expected_reward_wei: bad amount %q", raw)
				}
				publisher.MinReward = minReward
			}
			log.Info("commitment publisher enabled",
				"address", publisher.Address().Hex(), "mode", publisherMode(cfg.Registry),
				"min_expected_reward_wei", cfg.Registry.Publisher.MinExpectedReward)
		}
	}

	srv := &http.Server{
		Addr: cfg.API.Listen,
		Handler: api.New(api.Deps{
			Store:             st,
			Chains:            set,
			StartChain:        sv.StartStored,
			StopChain:         sv.Stop,
			ENS:               ensResolver,
			ENSParent:         cfg.Registry.ENSParent,
			TokenFilters:      tokenFilters,
			TokenAddresses:    tokenAddrs,
			Registry:          regClient,
			RegistryChainID:   cfg.Registry.ChainID,
			Publisher:         publisher,
			Relay:             relay,
			AllowRegistration: cfg.API.AllowRegistration,
			AuthToken:         cfg.API.AuthToken,
			Cost:              cfg.Cost,
			CORSOrigin:        cfg.API.CORSOrigin,
			WebDir:            webDir,
			Log:               log,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if mirror != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mirror.Run(runCtx, cfg.Registry.SyncInterval.D())
		}()
	}

	if publisher != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			publisherLoop(runCtx, publisher, set, cfg, log)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("api listening", "addr", cfg.API.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server stopped", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)

	cancel()
	wg.Wait()
	return nil
}

// publisherLoop keeps commitments moving through their lifecycle.
//
// On start it resumes any submission the previous process left waiting. Then, if
// auto-publish is on, each tick: settle what is still pending, finalize what is past
// its challenge window, claim the coverage reward on what has finalized (which is
// what pays the publisher back), and post a new commitment only when the index or
// its coverage has actually changed and the registry says it is worth posting.
func publisherLoop(ctx context.Context, p *hintreg.Publisher, set *chainset.Set, cfg *config.Config, log *slog.Logger) {
	for _, id := range set.IDs() {
		if _, err := p.ResumePending(ctx, id); err != nil && ctx.Err() == nil {
			log.Error("resume pending submissions failed", "chain_id", id, "err", err)
		}
	}
	if cfg.Registry.AutoPublishInterval <= 0 {
		return
	}

	t := time.NewTicker(cfg.Registry.AutoPublishInterval.D())
	defer t.Stop()

	for {
		// Settle before waiting, not after. A ticker does not fire until a full
		// interval has passed, so waiting first means every restart postpones
		// finalizing and claiming by the whole interval — and a deployment that
		// restarts more often than the interval would never settle anything at all.
		// Everything in a tick is idempotent: nothing is due, nothing is sent.
		for _, id := range set.IDs() {
			publishTick(ctx, p, id, cfg.Registry.CommitmentURI, log)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func publishTick(ctx context.Context, p *hintreg.Publisher, chainID uint64, uri string, log *slog.Logger) {
	pending, err := p.ResumePending(ctx, chainID)
	if err != nil {
		log.Error("resume pending submissions failed", "chain_id", chainID, "err", err)
		return
	}
	// Publishing is only half of the loop: a commitment stays challengeable, and its
	// bond stays locked, until someone settles it. In oracle mode this is also what
	// settles the assertion.
	if _, err := p.FinalizeDue(ctx, chainID); err != nil && ctx.Err() == nil {
		log.Error("finalize failed", "chain_id", chainID, "err", err)
	}
	if _, err := p.ClaimDue(ctx, chainID); err != nil && ctx.Err() == nil {
		log.Error("claim coverage reward failed", "chain_id", chainID, "err", err)
	}
	if pending > 0 {
		log.Info("submission still in flight; not building another", "chain_id", chainID, "pending", pending)
		return
	}

	e, err := p.Build(ctx, chainID, uri, false)
	// All four are ordinary reasons not to post, not failures: nothing to commit,
	// nothing new to commit, nobody paying for it, or a chain whose logs came
	// from a node we do not run and therefore cannot stake a bond on.
	if errors.Is(err, hintreg.ErrEmptyIndex) || errors.Is(err, hintreg.ErrUnchanged) ||
		errors.Is(err, hintreg.ErrUnfunded) || errors.Is(err, hintreg.ErrUntrusted) {
		return
	}
	if err != nil {
		log.Error("auto-publish build failed", "chain_id", chainID, "err", err)
		return
	}
	if _, err := p.Publish(ctx, e.ID); err != nil && ctx.Err() == nil {
		log.Error("auto-publish failed", "chain_id", chainID, "epoch", e.ID, "err", err)
	}
}

// newSubmitter picks how commitments get to the chain. Only the EOA mode exists;
// the switch is the seam a paymaster or relayer mode would plug into.
func newSubmitter(regSrc chain.Source, reg config.Registry) (hintreg.Submitter, error) {
	switch publisherMode(reg) {
	case config.PublisherModeEOA:
		key, err := parseKey(reg.PublisherKey)
		if err != nil {
			return nil, err
		}
		sender, ok := regSrc.(chain.Sender)
		if !ok {
			return nil, errors.New("registry chain source cannot send transactions")
		}
		sub := hintreg.NewEOASubmitter(sender, key, reg.ChainID, reg.Publisher.SubmitTimeout.D())
		sub.FallbackGas = reg.Publisher.FallbackGas
		return sub, nil
	default:
		return nil, fmt.Errorf("publisher mode %q is not implemented", reg.Publisher.Mode)
	}
}

func publisherMode(reg config.Registry) string {
	if reg.Publisher.Mode == "" {
		return config.PublisherModeEOA
	}
	return reg.Publisher.Mode
}

func parseKey(hexKey string) (*ecdsa.PrivateKey, error) {
	k, err := crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("parse publisher key: %w", err)
	}
	return k, nil
}
