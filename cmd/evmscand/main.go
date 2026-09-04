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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/api"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/indexer"
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
	services := map[uint64]*indexer.Service{}
	nudgers := map[uint64]api.Nudger{}
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
		svc := indexer.New(node, st, c.ChainID, indexer.Options{
			ChainName:        c.Name,
			Confirmations:    c.Confirmations,
			BackfillWindow:   c.BackfillWindow,
			TailWindow:       c.TailWindow,
			PollInterval:     c.PollInterval.D(),
			BackfillInterval: c.BackfillInterval.D(),
		}, log)
		services[c.ChainID] = svc
		nudgers[c.ChainID] = svc

		log.Info("chain ready",
			"chain_id", c.ChainID, "name", c.Name,
			"node", node.Endpoint().String(), "confirmations", c.Confirmations)
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
		log.Info("hint registry mirrored", "address", addr.Hex(), "chain_id", cfg.Registry.ChainID)

		if cfg.Registry.PublisherKey != "" {
			key, err := parseKey(cfg.Registry.PublisherKey)
			if err != nil {
				return err
			}
			sender, ok := regSrc.(chain.Sender)
			if !ok {
				return errors.New("registry chain source cannot send transactions")
			}
			publisher = hintreg.NewPublisher(regClient, sender, st, key, cfg.Registry.ChainID, log)
			log.Info("commitment publisher enabled", "address", publisher.Address().Hex())
		}
	}

	srv := &http.Server{
		Addr: cfg.API.Listen,
		Handler: api.New(api.Deps{
			Store:             st,
			Sources:           sources,
			Nudgers:           nudgers,
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
				log.Error("indexer stopped", "chain_id", id, "err", err)
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

	if publisher != nil && cfg.Registry.AutoPublishInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			autoPublish(runCtx, publisher, cfg, log)
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

// autoPublish periodically commits the index on-chain.
func autoPublish(ctx context.Context, p *hintreg.Publisher, cfg *config.Config, log *slog.Logger) {
	t := time.NewTicker(cfg.Registry.AutoPublishInterval.D())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		for _, c := range cfg.Chains {
			e, err := p.Build(ctx, c.ChainID, cfg.Registry.CommitmentURI)
			if errors.Is(err, hintreg.ErrEmptyIndex) {
				continue
			}
			if err != nil {
				log.Error("auto-publish build failed", "chain_id", c.ChainID, "err", err)
				continue
			}
			if _, err := p.Publish(ctx, e.ID); err != nil {
				log.Error("auto-publish failed", "chain_id", c.ChainID, "epoch", e.ID, "err", err)
			}
		}
	}
}

func parseKey(hexKey string) (*ecdsa.PrivateKey, error) {
	k, err := crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("parse publisher key: %w", err)
	}
	return k, nil
}
