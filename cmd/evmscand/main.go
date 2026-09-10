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
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/indexer"
	"github.com/Skanislav/evm-scan/internal/price"
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

	sources := map[uint64]chain.Source{}
	// Config order, kept because ranging the map above does not preserve it and the
	// API needs to know which chain a request without an explicit chain_id means.
	var chainOrder []uint64
	services := map[uint64]*indexer.Service{}
	workers := map[uint64]api.Worker{}
	pricers := map[uint64]*price.Pricer{}
	defer func() {
		for _, s := range sources {
			s.Close()
		}
	}()

	for _, c := range cfg.Chains {
		node, err := chain.Dial(ctx, c.Node, c.RequireLocal())
		if err != nil {
			return err
		}

		// A node pointed at the wrong network would silently produce a wrong index,
		// so verify rather than trust the config.
		actual, err := node.ChainID(ctx)
		if err != nil {
			node.Close()
			return fmt.Errorf("chain %d: read chain id: %w", c.ChainID, err)
		}
		if actual != c.ChainID {
			node.Close()
			return fmt.Errorf("chain %d: node at %s reports chain id %d", c.ChainID, c.Node, actual)
		}

		sources[c.ChainID] = node
		chainOrder = append(chainOrder, c.ChainID)
		svc := indexer.New(node, st, c.ChainID, indexer.Options{
			ChainName:        c.Name,
			Confirmations:    c.Confirmations,
			BackfillWindow:   c.BackfillWindow,
			TailWindow:       c.TailWindow,
			PollInterval:     c.PollInterval.D(),
			BackfillInterval: c.BackfillInterval.D(),
			Discovery: indexer.DiscoveryOptions{
				Enabled:              c.Discovery.Enabled,
				Lookback:             c.Discovery.Lookback,
				MaxBlocksPerTick:     c.Discovery.MaxBlocksPerTick,
				Interval:             c.Discovery.Interval.D(),
				AutoPromote:          c.Discovery.AutoPromote,
				MinEvents:            c.Discovery.MinEvents,
				MinBlocks:            c.Discovery.MinBlocks,
				MaxPromotionsPerTick: c.Discovery.MaxPromotionsPerTick,
			},
		}, log)
		services[c.ChainID] = svc
		workers[c.ChainID] = svc

		log.Info("chain ready",
			"chain_id", c.ChainID, "name", c.Name,
			"node", node.Endpoint().String(), "confirmations", c.Confirmations,
			"discovery", c.Discovery.Enabled, "auto_promote", c.Discovery.AutoPromote)

		// Prices come off the same node, through the same deployless trick, from
		// whatever oracles and pools the chain has. Where they come from is worth
		// a log line: it is the only third-party code in the read path.
		if p := newPricer(node, c, log); p != nil {
			pricers[c.ChainID] = p
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
	}

	var (
		regClient *hintreg.Client
		publisher *hintreg.Publisher
		mirror    *hintreg.Mirror
	)

	if addr, ok := cfg.RegistryAddress(); ok {
		regSrc := sources[cfg.Registry.ChainID]
		regClient, err = hintreg.NewClient(regSrc, addr)
		if err != nil {
			return err
		}

		head := func(ctx context.Context, chainID uint64) (uint64, bool, error) {
			src, ok := sources[chainID]
			if !ok {
				return 0, false, nil
			}
			h, err := src.HeadBlock(ctx)
			return h, true, err
		}

		nudgeMap := map[uint64]hintreg.Nudger{}
		for id, svc := range services {
			nudgeMap[id] = svc
		}
		mirror = hintreg.NewMirror(regClient, st, cfg.Registry.ChainID, head, nudgeMap, log)

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
			Sources:           sources,
			ChainOrder:        chainOrder,
			Workers:           workers,
			Pricers:           pricers,
			Registry:          regClient,
			RegistryChainID:   cfg.Registry.ChainID,
			Publisher:         publisher,
			AllowRegistration: cfg.API.AllowRegistration,
			CORSOrigin:        cfg.API.CORSOrigin,
			WebDir:            webDir,
			Log:               log,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for id, svc := range services {
		wg.Add(1)
		go func(id uint64, svc *indexer.Service) {
			defer wg.Done()
			if err := svc.Run(runCtx); err != nil && runCtx.Err() == nil {
				// An indexer that cannot run is a process that cannot do its job.
				// Exit so the supervisor restarts it, rather than serving stale
				// answers behind a healthy-looking API.
				log.Error("indexer stopped", "chain_id", id, "err", err)
				cancel()
			}
		}(id, svc)
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
			publisherLoop(runCtx, publisher, cfg, log)
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
func publisherLoop(ctx context.Context, p *hintreg.Publisher, cfg *config.Config, log *slog.Logger) {
	for _, c := range cfg.Chains {
		if _, err := p.ResumePending(ctx, c.ChainID); err != nil && ctx.Err() == nil {
			log.Error("resume pending submissions failed", "chain_id", c.ChainID, "err", err)
		}
	}
	if cfg.Registry.AutoPublishInterval <= 0 {
		return
	}

	t := time.NewTicker(cfg.Registry.AutoPublishInterval.D())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		for _, c := range cfg.Chains {
			publishTick(ctx, p, c.ChainID, cfg.Registry.CommitmentURI, log)
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
	if errors.Is(err, hintreg.ErrEmptyIndex) || errors.Is(err, hintreg.ErrUnchanged) || errors.Is(err, hintreg.ErrUnfunded) {
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
