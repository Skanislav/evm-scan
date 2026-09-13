// evmscan-state verifies portable snapshots and restores immutable history.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/userstate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	in := flag.String("in", "-", "snapshot or complete checkpoint export; - reads stdin")
	restore := flag.Bool("import", false, "restore verified immutable history to EVMSCAN_DATABASE_DSN; never change live heads or demand")
	export := flag.Bool("export-checkpoint", false, "build and export current account heads from EVMSCAN_DATABASE_DSN")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var st *store.Store
	if *restore || *export {
		dsn := os.Getenv("EVMSCAN_DATABASE_DSN")
		if dsn == "" {
			return fmt.Errorf("EVMSCAN_DATABASE_DSN is required")
		}
		var err error
		st, err = store.Open(ctx, dsn)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.Migrate(ctx); err != nil {
			return err
		}
	}
	if *export {
		c, err := st.BuildStateCheckpoint(ctx)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(c)
	}
	var r io.Reader = os.Stdin
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, 256<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 256<<20 {
		return fmt.Errorf("export exceeds 256 MiB")
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		return err
	}
	if _, ok := shape["accounts"]; ok {
		var c userstate.Checkpoint
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if err := c.Validate(); err != nil {
			return err
		}
		if *restore {
			if err := st.ImportCheckpoint(ctx, c); err != nil {
				return err
			}
		}
		fmt.Printf("verified checkpoint %s (%d accounts); imported=%t\n", c.Root.Hex(), len(c.Accounts), *restore)
		return nil
	}
	var s userstate.Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	id, _, err := s.Validate()
	if err != nil {
		return err
	}
	if *restore {
		if err := st.ImportState(ctx, s); err != nil {
			return err
		}
	}
	fmt.Printf("verified account %s revision %s id %s root %s; imported=%t\n", s.Account.Hex(), s.Revision, id.Hex(), s.StateRoot.Hex(), *restore)
	return nil
}
