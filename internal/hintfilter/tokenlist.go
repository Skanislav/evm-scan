package hintfilter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Token lists are the tokenlists.org schema, which is what Uniswap, Sushi, CoW and
// Coingecko all publish. Only three fields are read. A list is a hint about which
// contracts are worth looking at, never a source of truth about them: symbol,
// decimals and everything else still come from the chain via internal/token, so a
// wrong or hostile list costs a wasted read and can never put a bad number in front
// of anyone.

// TokenList is a tokenlists.org document.
type TokenList struct {
	Name      string        `json:"name"`
	Timestamp string        `json:"timestamp"`
	Version   ListVersion   `json:"version"`
	Tokens    []ListedToken `json:"tokens"`
}

type ListVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

func (v ListVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

type ListedToken struct {
	ChainID  uint64 `json:"chainId"`
	Address  string `json:"address"`
	Name     string `json:"name"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

// maxListBytes bounds what a remote list can make us allocate. The largest real
// lists are a few megabytes; this leaves room without letting a redirect to
// something enormous take the process down.
const maxListBytes = 64 << 20

// FetchTokenList reads a token list from an http(s) URL or a local path.
//
// The one existing HTTP fetch in this repo (snapshot.Open) uses http.DefaultClient
// with no timeout and no size cap. That is survivable for an operator-run recovery
// and not for something on a request path, so this sets both.
func FetchTokenList(ctx context.Context, source string) (*TokenList, error) {
	var r io.ReadCloser

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("user-agent", "evmscan-hint/1")

		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", source, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: %s", source, resp.Status)
		}
		r = resp.Body
	} else {
		f, err := os.Open(source)
		if err != nil {
			return nil, err
		}
		r = f
	}
	defer r.Close()

	var list TokenList
	if err := json.NewDecoder(io.LimitReader(r, maxListBytes)).Decode(&list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	if len(list.Tokens) == 0 {
		return nil, fmt.Errorf("parse %s: list has no tokens", source)
	}
	return &list, nil
}

// Addresses returns the list's tokens for one chain, deduped and in list order.
//
// Addresses that are not 20 bytes of hex are skipped rather than rejected: lists
// are community-maintained and one malformed entry should not cost the other fifty
// thousand.
func (l *TokenList) Addresses(chainID uint64) []common.Address {
	seen := make(map[common.Address]bool, len(l.Tokens))
	out := make([]common.Address, 0, len(l.Tokens))
	for _, t := range l.Tokens {
		if t.ChainID != chainID {
			continue
		}
		if !common.IsHexAddress(t.Address) {
			continue
		}
		a := common.HexToAddress(t.Address)
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// FetchFilter reads an encoded filter from an http(s) URL or a local path.
func FetchFilter(ctx context.Context, source string) (*Filter, error) {
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		raw, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		return Decode(raw)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("user-agent", "evmscan-hint/1")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", source, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes))
	if err != nil {
		return nil, err
	}
	return Decode(raw)
}
