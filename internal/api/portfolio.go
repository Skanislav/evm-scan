package api

import (
	"context"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/lens"
	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/token"
)

// The portfolio endpoint is the read side of the same premise as the rest of the
// service: the answer comes from your own node, in one call, and says exactly which
// block it is true at. What the index contributes is only *which* contracts to ask
// about — every number below is read live through the deployless lens, not stored.

type accountStateJSON struct {
	Balance string `json:"balance"`
	// Nonce comes from eth_getTransactionCount: no contract can read another
	// account's nonce, so it is the one field here that is not from the lens.
	Nonce      *uint64 `json:"nonce,omitempty"`
	IsContract bool    `json:"is_contract"`
	CodeSize   uint64  `json:"code_size"`
	CodeHash   string  `json:"code_hash,omitempty"`
	// IsDelegated reports an EIP-7702 delegation: an EOA that executes Delegate's
	// code. Worth surfacing prominently — it is the difference between "this address
	// is a key" and "this address runs somebody else's contract".
	IsDelegated bool   `json:"is_delegated"`
	Delegate    string `json:"delegate,omitempty"`
	Code        string `json:"code,omitempty"`
}

type allowanceJSON struct {
	Spender string `json:"spender"`
	// Amount is absent when the token did not answer, which is not the same as zero.
	Amount         *string `json:"amount,omitempty"`
	ApprovedForAll bool    `json:"approved_for_all,omitempty"`
}

type tokenIDJSON struct {
	ID           string  `json:"id"`
	Owner        *string `json:"owner,omitempty"`
	Balance      *string `json:"balance,omitempty"`
	Approved     *string `json:"approved,omitempty"`
	URI          string  `json:"uri,omitempty"`
	URITruncated bool    `json:"uri_truncated,omitempty"`
}

type portfolioTokenJSON struct {
	Address        string          `json:"address"`
	Standard       string          `json:"standard"`
	IsContract     bool            `json:"is_contract"`
	SupportsERC165 bool            `json:"supports_erc165"`
	Symbol         string          `json:"symbol,omitempty"`
	Name           string          `json:"name,omitempty"`
	Decimals       *int16          `json:"decimals,omitempty"`
	TotalSupply    *string         `json:"total_supply,omitempty"`
	Balance        *string         `json:"balance,omitempty"`
	Allowances     []allowanceJSON `json:"allowances,omitempty"`
	IDs            []tokenIDJSON   `json:"ids,omitempty"`
	// IndexStatus is what the indexer knows about this contract, when it knows
	// anything: the lens will happily read a contract nobody registered.
	IndexStatus string `json:"index_status,omitempty"`
}

// accountPortfolio reads an account's live position through the deployless lens.
//
// Without an explicit token list it reads what discovery already found for this
// account, which is the pairing the whole project is built around: the index says
// where to look, the node says what is there.
func (s *Server) accountPortfolio(w http.ResponseWriter, r *http.Request) {
	chainID, src, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	account, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}

	q := r.URL.Query()
	spenders, err := parseAddressList(q.Get("spenders"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad spenders", err)
		return
	}
	ids, err := parseIDList(q.Get("ids"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad ids", err)
		return
	}

	ctx := r.Context()
	req := lens.Request{
		Account:        account,
		Spenders:       spenders,
		IncludeURI:     q.Get("uri") == "true",
		IncludeCode:    q.Get("code") == "true",
		EnumerateLimit: uint64(intParam(r, "nfts", 0, 0, 256)),
	}

	// Explicit tokens win; otherwise the index supplies the list.
	status := map[common.Address]string{}
	if raw := q.Get("tokens"); raw != "" {
		list, err := parseAddressList(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad tokens", err)
			return
		}
		for _, addr := range list {
			req.Tokens = append(req.Tokens, lens.TokenQuery{Token: addr, IDs: ids})
		}
	} else {
		rows, err := s.d.Store.AccountAssets(ctx, chainID, account)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "query failed", err)
			return
		}
		for _, a := range rows {
			req.Tokens = append(req.Tokens, lens.TokenQuery{Token: a.Asset, IDs: ids})
			status[a.Asset] = a.Status
		}
	}
	if len(req.Tokens) > 512 {
		writeErr(w, http.StatusBadRequest, "too many tokens", nil)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := lens.Query(ctx, src, req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "node read failed", err)
		return
	}

	tokens := make([]portfolioTokenJSON, len(res.Tokens))
	for i, t := range res.Tokens {
		tokens[i] = portfolioTokenJSON{
			Address:        t.Address.Hex(),
			Standard:       t.Standard.String(),
			IsContract:     t.IsContract,
			SupportsERC165: t.SupportsERC165,
			Symbol:         t.Symbol,
			Name:           t.Name,
			Decimals:       t.Decimals,
			TotalSupply:    numOrNil(t.TotalSupply),
			Balance:        numOrNil(t.Balance),
			IndexStatus:    status[t.Address],
		}
		for _, a := range t.Allowances {
			tokens[i].Allowances = append(tokens[i].Allowances, allowanceJSON{
				Spender:        a.Spender.Hex(),
				Amount:         numOrNil(a.Amount),
				ApprovedForAll: a.ApprovedForAll,
			})
		}
		for _, id := range t.IDs {
			tokens[i].IDs = append(tokens[i].IDs, tokenIDJSON{
				ID:           id.ID.String(),
				Owner:        addrOrNil(id.Owner),
				Balance:      numOrNil(id.Balance),
				Approved:     addrOrNil(id.Approved),
				URI:          id.URI,
				URITruncated: id.URITruncated,
			})
		}
	}

	state := accountStateJSON{
		Balance:     res.Account.Balance.String(),
		IsContract:  res.Account.IsContract,
		CodeSize:    res.Account.CodeSize,
		CodeHash:    res.Account.CodeHash.Hex(),
		IsDelegated: res.Account.IsDelegated,
	}
	if res.Account.NonceKnown {
		nonce := res.Account.Nonce
		state.Nonce = &nonce
	}
	if res.Account.IsDelegated {
		state.Delegate = res.Account.Delegate.Hex()
	}
	if len(res.Account.Code) > 0 {
		state.Code = "0x" + common.Bytes2Hex(res.Account.Code)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"account":     account.Hex(),
		"chain_id":    res.ChainID,
		"as_of_block": res.BlockNumber,
		"parent_hash": res.ParentHash.Hex(),
		"timestamp":   res.Timestamp,
		// Atomic false means the token list was too large for one reply and the head
		// moved while it was being read, so this is a stitched view rather than a
		// snapshot of a single block.
		"atomic":        res.Atomic,
		"calls":         res.Calls,
		"account_state": state,
		"tokens":        tokens,
		"read_by":       "deployless AssetLens (eth_call, no deployment); live state, not indexed",
	})
}

// fillBalances reads every discovered asset's balance in one deployless call.
//
// It returns the block that read ran at, which is better than asking for the head
// separately: this one is the block the numbers are actually from.
//
// ERC-1155 is skipped: balance there is per token id, and the index tracks contracts
// rather than ids, so there is no single number to report. Returning nothing is
// better than returning a misleading zero.
func (s *Server) fillBalances(ctx context.Context, src chain.Source, account common.Address,
	rows []store.AccountAsset, out []accountAssetJSON) (block uint64, ok bool) {

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req := lens.Request{Account: account, SkipNonce: true}
	at := make([]int, 0, len(rows))
	for i := range rows {
		if out[i].Standard == "erc1155" || out[i].Standard == "unknown" {
			continue
		}
		req.Tokens = append(req.Tokens, lens.TokenQuery{Token: rows[i].Asset})
		at = append(at, i)
	}
	if len(req.Tokens) == 0 {
		return 0, false
	}

	res, err := lens.Query(ctx, src, req)
	if err != nil {
		// A node that will not run a deployless call still answers ordinary ones, so
		// fall back rather than dropping balances from the response.
		if s.d.Log != nil {
			s.d.Log.Warn("lens read failed, falling back to per-token calls",
				"account", account, "err", err)
		}
		s.fillBalancesOneByOne(ctx, src, account, req.Tokens, at, out)
		return 0, false
	}

	for j, t := range res.Tokens {
		i := at[j]
		if t.Balance == nil {
			out[i].BalanceError = "contract did not answer balanceOf"
			continue
		}
		str := t.Balance.String()
		out[i].Balance = &str
	}
	return res.BlockNumber, res.Atomic
}

// fillBalancesOneByOne is the fallback path: one eth_call per token, as before.
func (s *Server) fillBalancesOneByOne(ctx context.Context, src chain.Source, account common.Address,
	tokens []lens.TokenQuery, at []int, out []accountAssetJSON) {

	for j, t := range tokens {
		i := at[j]
		bal, err := token.BalanceOf(ctx, src, t.Token, account)
		if err != nil {
			out[i].BalanceError = err.Error()
			continue
		}
		str := bal.String()
		out[i].Balance = &str
	}
}

func numOrNil(v *big.Int) *string {
	if v == nil {
		return nil
	}
	s := v.String()
	return &s
}

func addrOrNil(a *common.Address) *string {
	if a == nil {
		return nil
	}
	s := a.Hex()
	return &s
}

func parseAddressList(raw string) ([]common.Address, error) {
	var out []common.Address
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, err := parseAddress(part)
		if err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, nil
}

func parseIDList(raw string) ([]*big.Int, error) {
	var out []*big.Int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, ok := new(big.Int).SetString(part, 10)
		if !ok || v.Sign() < 0 {
			return nil, strconv.ErrSyntax
		}
		out = append(out, v)
	}
	return out, nil
}
