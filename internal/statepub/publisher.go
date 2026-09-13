// Package statepub checkpoints signed user state through an ENSv2 resolver.
package statepub

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/userstate"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const ChainID = 11155111
const RecordKey = "evmscan.states"

var ResolverABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"setText","stateMutability":"nonpayable","inputs":[{"name":"node","type":"bytes32"},{"name":"key","type":"string"},{"name":"value","type":"string"}],"outputs":[]},{"type":"function","name":"text","stateMutability":"view","inputs":[{"name":"node","type":"bytes32"},{"name":"key","type":"string"}],"outputs":[{"type":"string"}]}]`))
	if err != nil {
		panic(err)
	}
	return a
}()

type Node interface {
	chain.Source
	chain.Sender
}
type Publisher struct {
	Store    *store.Store
	Node     Node
	Key      *ecdsa.PrivateKey
	Resolver common.Address
	Namehash common.Hash
	Interval time.Duration
	MaxGas   uint64
	MaxFee   *big.Int
	Log      *slog.Logger
}

func ReadRecord(ctx context.Context, n chain.Source, resolver common.Address, node common.Hash, key string) (string, error) {
	data, err := ResolverABI.Pack("text", node, key)
	if err != nil {
		return "", err
	}
	raw, err := n.CallAtHead(ctx, ethereum.CallMsg{To: &resolver, Data: data})
	if err != nil {
		return "", err
	}
	v, err := ResolverABI.Unpack("text", raw)
	if err != nil {
		return "", err
	}
	if len(v) != 1 {
		return "", errors.New("invalid resolver response")
	}
	return v[0].(string), nil
}
func (p *Publisher) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var next time.Time
	for {
		build := !time.Now().Before(next)
		if build {
			next = time.Now().Add(interval)
		}
		tickCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := p.Tick(tickCtx, build)
		cancel()
		if err != nil && ctx.Err() == nil {
			p.Log.Warn("state checkpoint pending", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (p *Publisher) Tick(ctx context.Context, build bool) error {
	if p.Key == nil || p.MaxGas == 0 || p.MaxFee == nil || p.MaxFee.Sign() <= 0 {
		return errors.New("state publisher requires key and positive gas/fee caps")
	}
	id, err := p.Node.ChainID(ctx)
	if err != nil {
		return err
	}
	if id != ChainID {
		return fmt.Errorf("state publisher requires ENSv2 Sepolia, got %d", id)
	}
	// A session lock coordinates publishers across daemon processes sharing this DB.
	conn, err := p.Store.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(736492817201)`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(c, `SELECT pg_advisory_unlock(736492817201)`); err != nil {
			_ = conn.Conn().Close(c)
		}
	}()
	jobs, err := p.Store.StatePublications(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Status == "pending" {
			return p.settle(ctx, j)
		}
	}
	// Recheck the latest confirmed receipt even when there is nothing to publish.
	if len(jobs) > 0 && jobs[0].Status == "confirmed" {
		j := jobs[0]
		r, err := p.Node.TransactionReceipt(ctx, j.Hash)
		if err != nil && !errors.Is(err, ethereum.NotFound) {
			return err
		}
		if err != nil || r == nil {
			return p.Store.UpdateStatePublication(ctx, j.ID, "pending", "receipt disappeared; checking reorganization")
		}
		canonical, err := p.Node.HeaderHash(ctx, r.BlockNumber.Uint64())
		if err != nil {
			return err
		}
		if canonical != r.BlockHash {
			return p.Store.UpdateStatePublication(ctx, j.ID, "pending", "receipt is no longer canonical")
		}
	}
	if !build {
		return nil
	}
	c, err := p.Store.BuildStateCheckpoint(ctx)
	if err != nil {
		return err
	}
	if len(c.Accounts) == 0 {
		return nil
	}
	current, err := ReadRecord(ctx, p.Node, p.Resolver, p.Namehash, RecordKey)
	if err != nil {
		return err
	}
	record := userstate.Record("states", c.Root)
	if current == record {
		return nil
	}
	data, err := ResolverABI.Pack("setText", p.Namehash, RecordKey, record)
	if err != nil {
		return err
	}
	from := crypto.PubkeyToAddress(p.Key.PublicKey)
	msg := ethereum.CallMsg{From: from, To: &p.Resolver, Data: data}
	gas, err := p.Node.EstimateGas(ctx, msg)
	if err != nil {
		return fmt.Errorf("estimate ENS write (check record permission): %w", err)
	}
	if gas > p.MaxGas || gas > ^uint64(0)/12 {
		return errors.New("ENS estimate exceeds configured gas cap")
	}
	gas = gas * 12 / 10
	if gas > p.MaxGas {
		gas = p.MaxGas
	}
	price, err := p.Node.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}
	if price.Sign() <= 0 || price.Cmp(p.MaxFee) > 0 {
		return errors.New("ENS fee exceeds configured cap")
	}
	nonce, err := p.Node.PendingNonceAt(ctx, from)
	if err != nil {
		return err
	}
	tx, err := types.SignTx(types.NewTransaction(nonce, p.Resolver, new(big.Int), gas, price, data), types.LatestSignerForChainID(big.NewInt(ChainID)), p.Key)
	if err != nil {
		return err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return err
	}
	j := store.StatePublication{Root: c.Root, Resolver: p.Resolver, Node: p.Namehash, Raw: raw, Hash: tx.Hash(), Status: "pending"}
	j.ID, err = p.Store.SaveStatePublication(ctx, j)
	if err != nil {
		return err
	}
	return p.settle(ctx, j)
}
func (p *Publisher) settle(ctx context.Context, j store.StatePublication) error {
	receipt, err := p.Node.TransactionReceipt(ctx, j.Hash)
	if errors.Is(err, ethereum.NotFound) || (err == nil && receipt == nil) {
		var tx types.Transaction
		if err := tx.UnmarshalBinary(j.Raw); err != nil {
			return err
		}
		if tx.ChainId().Cmp(big.NewInt(ChainID)) != 0 || tx.Hash() != j.Hash {
			return errors.New("persisted transaction identity mismatch")
		}
		err = p.Node.SendTransaction(ctx, &tx)
		message := "awaiting receipt"
		if err != nil {
			message = err.Error()
		}
		if e := p.Store.UpdateStatePublication(ctx, j.ID, "pending", message); e != nil {
			return e
		}
		return err
	}
	if err != nil {
		return err
	}
	head, err := p.Node.HeadBlock(ctx)
	if err != nil {
		return err
	}
	height := receipt.BlockNumber.Uint64()
	canonical, err := p.Node.HeaderHash(ctx, height)
	if err != nil {
		return err
	}
	if canonical != receipt.BlockHash {
		return p.Store.UpdateStatePublication(ctx, j.ID, "pending", "waiting for canonical receipt")
	}
	if head < height || head-height < 12 {
		return nil
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return p.Store.UpdateStatePublication(ctx, j.ID, "failed", "ENS write reverted")
	}
	record, err := ReadRecord(ctx, p.Node, j.Resolver, j.Node, RecordKey)
	if err != nil {
		return err
	}
	if record != userstate.Record("states", j.Root) {
		return p.Store.UpdateStatePublication(ctx, j.ID, "failed", "ENS record differs at readback")
	}
	return p.Store.UpdateStatePublication(ctx, j.ID, "confirmed", "")
}
