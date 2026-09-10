package hintreg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/snapshot"
	"github.com/Skanislav/evm-scan/internal/store"
)

// ErrEmptyIndex is returned when there is nothing to commit to.
var ErrEmptyIndex = errors.New("hintreg: index is empty, nothing to commit")

// ErrUnchanged is returned when both the index root and the coverage root are
// identical to the last commitment. Posting it again would cost a bond, tell
// consumers nothing new and earn nothing.
var ErrUnchanged = errors.New("hintreg: index and coverage unchanged since last commitment")

// ErrUntrusted is returned for a chain whose logs came from a node this deployment
// does not run.
//
// This is the check that makes the trust level mean something. A commitment is
// bonded: posting one stakes real money on the claim that these logs are what the
// chain actually emitted. For a chain read over somebody else's RPC we have no
// way to know that — the provider could have omitted a log, and the first anyone
// would learn of it is a successful challenge. Indexing such a chain and serving
// it is fine, and useful; committing to it is not ours to do until an operator
// says otherwise, deliberately, by promoting the chain.
//
// Unlike ErrUnchanged, force does not override this. Forcing is for "post it
// anyway, I know the root is the same"; there is no corresponding "stake the bond
// anyway" worth having behind a boolean.
var ErrUntrusted = errors.New("hintreg: chain is not verified; its data may not back a bonded commitment")

// ErrUnfunded is returned when the registry quotes less for an epoch's coverage than
// the publisher's configured floor. Nothing is stored: the index has not earned a
// commitment yet.
var ErrUnfunded = errors.New("hintreg: coverage is worth less than the configured minimum; not posting")

// registryReader is what the publisher needs from the registry client. An interface so
// tests can run without a node.
type registryReader interface {
	Address() common.Address
	ABI() abi.ABI
	Mode(ctx context.Context) (Mode, error)
	GetEpoch(ctx context.Context, id int64) (RegistryEpoch, error)
	Claimable(ctx context.Context, key common.Hash, fromBlock, toBlock uint64) (*big.Int, error)
	Allowance(ctx context.Context, token, owner, spender common.Address) (*big.Int, error)
	Simulate(ctx context.Context, from common.Address, method string, args ...any) error
}

// epochStore is the persistence the publisher touches. *store.Store satisfies it.
type epochStore interface {
	CoverageRange(ctx context.Context, chainID uint64) (from, to uint64, err error)
	ListCursors(ctx context.Context, chainID uint64) ([]store.Cursor, error)
	SnapshotIndex(ctx context.Context, chainID, toBlock uint64) ([]store.AccountAssetSet, error)
	CreateEpoch(ctx context.Context, e store.Epoch, leaves []store.EpochLeaf, coverage []store.EpochCoverage) (int64, error)
	GetEpoch(ctx context.Context, id int64) (store.Epoch, error)
	EpochCoverage(ctx context.Context, epochID int64) ([]store.EpochCoverage, error)
	MarkSubmitted(ctx context.Context, id int64, ref common.Hash) error
	MarkPublished(ctx context.Context, id int64, onchainID int64, txHash common.Hash) error
	MarkClaimed(ctx context.Context, id int64, tx common.Hash, rewardWei string) error
	SetEpochStatus(ctx context.Context, id int64, status string) error
	SetEpochURI(ctx context.Context, id int64, uri string) error
	PendingSubmissions(ctx context.Context, chainID uint64) ([]store.Epoch, error)
	PublishedUnfinalized(ctx context.Context, chainID uint64) ([]store.Epoch, error)
	UnclaimedFinalized(ctx context.Context, chainID uint64) ([]store.Epoch, error)
	LatestCommittedRoots(ctx context.Context, chainID uint64) (root, coverage common.Hash, ok bool, err error)
	// GetChainProfile is read for one field, trust, and it is the field that
	// decides whether this chain's data may sit behind a bond at all.
	GetChainProfile(ctx context.Context, chainID uint64) (store.ChainProfile, error)
}

// Publisher builds merkle commitments over the local index and posts them on-chain.
type Publisher struct {
	client registryReader
	sub    Submitter
	st     epochStore
	log    *slog.Logger

	// StaleAfter is how long a submission may sit unconfirmed before it is written
	// off as failed. Zero disables the write-off.
	StaleAfter time.Duration
	// FinalizeSlack is added to a challenge deadline before finalizing, so a clock a
	// few seconds ahead of the chain does not send a transaction that reverts.
	FinalizeSlack time.Duration
	// MinReward is the least the registry must quote for an epoch's coverage before
	// Build will store it. Nil or zero disables the check. This is what keeps a
	// publisher from fronting gas for an epoch nobody paid for.
	MinReward *big.Int

	now func() time.Time
}

// NewPublisher wires a publisher to whatever will carry its transactions.
func NewPublisher(c *Client, sub Submitter, st *store.Store, log *slog.Logger) *Publisher {
	return newPublisher(c, sub, st, log)
}

func newPublisher(c registryReader, sub Submitter, st epochStore, log *slog.Logger) *Publisher {
	return &Publisher{
		client:        c,
		sub:           sub,
		st:            st,
		log:           log.With("publisher", sub.Sender().Hex()),
		StaleAfter:    30 * time.Minute,
		FinalizeSlack: 15 * time.Second,
		now:           time.Now,
	}
}

// Address is the publisher's on-chain identity.
func (p *Publisher) Address() common.Address { return p.sub.Sender() }

// Build computes a commitment over the index as of the chain's current coverage and
// stores it locally. It reads the registry to price the coverage but sends nothing.
//
// A commitment is a cumulative snapshot rather than a delta: one root answers "which
// contracts has this account ever touched", which is the question a wallet actually
// asks. Deltas would force consumers to walk every epoch to get the same answer.
//
// Alongside the index root it commits a coverage root: one leaf per scanned asset
// with the block range the publisher stands behind. That is what the registry pays
// for, so the coverage is derived from the scan cursors, not from the interactions
// that happened to occur.
//
// Unless force is set, an epoch whose index root and coverage root both match the
// last commitment is refused with ErrUnchanged. When MinReward is set, an epoch the
// registry values below it is refused with ErrUnfunded; nothing is stored either way.
// A chain whose data came from a node this deployment does not run is refused with
// ErrUntrusted, and force does not override that one.
func (p *Publisher) Build(ctx context.Context, chainID uint64, uri string, force bool) (store.Epoch, error) {
	if err := p.checkTrusted(ctx, chainID); err != nil {
		return store.Epoch{}, err
	}

	from, to, err := p.st.CoverageRange(ctx, chainID)
	if err != nil {
		return store.Epoch{}, err
	}

	sets, err := p.st.SnapshotIndex(ctx, chainID, to)
	if err != nil {
		return store.Epoch{}, err
	}
	if len(sets) == 0 {
		return store.Epoch{}, ErrEmptyIndex
	}

	leaves := make([]common.Hash, len(sets))
	rows := make([]store.EpochLeaf, len(sets))
	for i, s := range sets {
		ah := merkle.AssetsHash(s.Assets)
		leaf := merkle.LeafHash(s.Account, chainID, ah)
		leaves[i] = leaf
		rows[i] = store.EpochLeaf{Index: i, Account: s.Account, AssetsHash: ah, Leaf: leaf, Assets: s.Assets}
	}
	root := merkle.Build(leaves).Root()

	cursors, err := p.st.ListCursors(ctx, chainID)
	if err != nil {
		return store.Epoch{}, err
	}
	coverage := CoverageFromCursors(chainID, cursors, to)
	covRoot := coverageRoot(coverage)
	for _, c := range coverage {
		// An asset scanned from below the earliest interaction widens the epoch,
		// since claims must sit inside the epoch's declared range.
		if c.FromBlock < from {
			from = c.FromBlock
		}
	}

	if !force {
		lastRoot, lastCov, ok, err := p.st.LatestCommittedRoots(ctx, chainID)
		if err != nil {
			return store.Epoch{}, err
		}
		if ok && lastRoot == root && lastCov == covRoot {
			return store.Epoch{}, ErrUnchanged
		}
	}

	expected, err := p.expectedReward(ctx, coverage)
	if err != nil {
		return store.Epoch{}, err
	}
	if p.MinReward != nil && p.MinReward.Sign() > 0 && expected.Cmp(p.MinReward) < 0 {
		p.log.Info("coverage not worth posting yet",
			"chain_id", chainID, "expected_reward_wei", expected, "min_reward_wei", p.MinReward)
		return store.Epoch{}, ErrUnfunded
	}

	e := store.Epoch{
		ChainID:           chainID,
		FromBlock:         from,
		ToBlock:           to,
		MerkleRoot:        root,
		CoverageRoot:      covRoot,
		LeafCount:         int64(len(leaves)),
		URI:               uri,
		Status:            store.EpochBuilt,
		ExpectedRewardWei: expected.String(),
	}

	id, err := p.st.CreateEpoch(ctx, e, rows, coverage)
	if err != nil {
		return store.Epoch{}, err
	}
	e.ID = id

	// The pointer to the published table names the epoch, and the epoch has no id
	// until the row above exists — so the template is expanded here rather than by
	// the caller. It has to be settled before Publish, because the contract takes the
	// uri as an argument and has no setter: an epoch published with the wrong string
	// carries it forever.
	if expanded := snapshot.URI(uri, chainID, id); expanded != uri {
		if err := p.st.SetEpochURI(ctx, id, expanded); err != nil {
			return store.Epoch{}, fmt.Errorf("record commitment uri: %w", err)
		}
		e.URI = expanded
	}

	p.log.Info("built index commitment", "epoch", id, "chain_id", chainID, "leaves", len(leaves),
		"from_block", from, "to_block", to, "root", e.MerkleRoot.Hex(),
		"assets_covered", len(coverage), "expected_reward_wei", expected)
	return e, nil
}

// CoverageFromCursors turns scan progress into coverage leaves: for each asset, the
// contiguous block range whose logs are in the rollup, capped at the epoch's top.
//
// The lower bound is where the backfill has reached (its floor once done); the upper
// bound is the follower's tail, which cannot exceed the epoch's confirmed top. An
// asset with nothing scanned yet contributes no leaf. Leaves are in cursor order,
// which ListCursors keeps sorted by address, so the tree is reproducible.
func CoverageFromCursors(chainID uint64, cursors []store.Cursor, to uint64) []store.EpochCoverage {
	var out []store.EpochCoverage
	for _, c := range cursors {
		lo := c.BackfillNext + 1
		if c.BackfillDone {
			lo = c.BackfillFloor
		}
		hi := min(c.TailBlock, to)
		if hi == 0 || lo > hi {
			continue
		}
		key := AssetKey(chainID, c.Address)
		out = append(out, store.EpochCoverage{
			Index:       len(out),
			Asset:       c.Address,
			RegistryKey: key,
			FromBlock:   lo,
			ToBlock:     hi,
			Leaf:        merkle.CoverageLeaf(key, lo, hi),
		})
	}
	return out
}

func coverageRoot(coverage []store.EpochCoverage) common.Hash {
	leaves := make([]common.Hash, len(coverage))
	for i, c := range coverage {
		leaves[i] = c.Leaf
	}
	return merkle.Build(leaves).Root()
}

// expectedReward asks the registry what each coverage leaf would pay if claimed now.
func (p *Publisher) expectedReward(ctx context.Context, coverage []store.EpochCoverage) (*big.Int, error) {
	total := new(big.Int)
	for _, c := range coverage {
		amt, err := p.client.Claimable(ctx, c.RegistryKey, c.FromBlock, c.ToBlock)
		if err != nil {
			return nil, fmt.Errorf("hintreg: quote coverage for %s: %w", c.Asset.Hex(), err)
		}
		total.Add(total, amt)
	}
	return total, nil
}

// Publish posts a previously built commitment to the registry and waits for its
// receipt. The submission reference is recorded before the wait, so if the wait is
// interrupted ResumePending picks it up rather than Publish sending it again.
//
// The bond depends on the registry's adjudication mode: wei attached to the call in
// local-arbiter mode, an ERC-20 allowance the registry pulls and forwards to the
// oracle in oracle mode. Publish reads the mode and does whichever applies.
func (p *Publisher) Publish(ctx context.Context, epochID int64) (common.Hash, error) {
	e, err := p.st.GetEpoch(ctx, epochID)
	if err != nil {
		return common.Hash{}, err
	}
	if e.Status != store.EpochBuilt {
		return common.Hash{}, fmt.Errorf("hintreg: epoch %d is %s, expected %s", epochID, e.Status, store.EpochBuilt)
	}

	mode, err := p.client.Mode(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	value := new(big.Int)
	if mode.OracleMode() {
		if err := p.ensureBondAllowance(ctx, mode); err != nil {
			return common.Hash{}, err
		}
	} else {
		value = mode.PublisherBond
	}

	data, err := p.client.ABI().Pack("publishIndex",
		e.ChainID, e.FromBlock, e.ToBlock, [32]byte(e.MerkleRoot), [32]byte(e.CoverageRoot), e.URI)
	if err != nil {
		return common.Hash{}, fmt.Errorf("hintreg: pack publishIndex: %w", err)
	}

	ref, err := p.sub.Submit(ctx, p.client.Address(), value, data)
	if err != nil {
		return common.Hash{}, fmt.Errorf("hintreg: submit publishIndex: %w", err)
	}
	if err := p.st.MarkSubmitted(ctx, epochID, ref); err != nil {
		return ref, err
	}
	p.log.Info("submitted index commitment", "epoch", epochID, "ref", ref.Hex(), "mode", mode.String())

	return ref, p.settle(ctx, epochID, ref)
}

// ensureBondAllowance approves the registry to move one bond of the oracle's currency.
//
// The approval is for exactly one bond rather than unlimited: the registry is the thing
// we are bonding against, and an unlimited allowance would let a future upgrade of it
// drain the publisher's balance.
func (p *Publisher) ensureBondAllowance(ctx context.Context, mode Mode) error {
	have, err := p.client.Allowance(ctx, mode.BondCurrency, p.sub.Sender(), p.client.Address())
	if err != nil {
		return err
	}
	if have.Cmp(mode.PublisherBond) >= 0 {
		return nil
	}

	erc20, err := ERC20ABI()
	if err != nil {
		return err
	}
	data, err := erc20.Pack("approve", p.client.Address(), mode.PublisherBond)
	if err != nil {
		return fmt.Errorf("hintreg: pack approve: %w", err)
	}
	ref, err := p.sub.Submit(ctx, mode.BondCurrency, nil, data)
	if err != nil {
		return fmt.Errorf("hintreg: submit approve: %w", err)
	}
	rcpt, err := p.sub.Wait(ctx, ref)
	if err != nil {
		return err
	}
	if rcpt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("hintreg: bond approval reverted (ref %s)", ref.Hex())
	}

	p.log.Info("approved oracle bond",
		"currency", mode.BondCurrency.Hex(), "amount", mode.PublisherBond.String(), "tx", rcpt.TxHash.Hex())
	return nil
}

// settle waits for a submission and records the on-chain epoch it produced.
func (p *Publisher) settle(ctx context.Context, epochID int64, ref common.Hash) error {
	rcpt, err := p.sub.Wait(ctx, ref)
	if err != nil {
		return err
	}
	if rcpt.Status != types.ReceiptStatusSuccessful {
		if err := p.st.SetEpochStatus(ctx, epochID, store.EpochFailed); err != nil {
			return err
		}
		return fmt.Errorf("hintreg: publishIndex reverted (ref %s)", ref.Hex())
	}

	onchainID, err := p.epochIDFromReceipt(rcpt)
	if err != nil {
		return err
	}
	if err := p.st.MarkPublished(ctx, epochID, onchainID, rcpt.TxHash); err != nil {
		return err
	}

	args := []any{"epoch", epochID, "onchain_epoch", onchainID, "tx", rcpt.TxHash.Hex()}
	// The assertion id is what a disputer needs to challenge us at the oracle, so it
	// belongs in the log even though nothing local keys off it.
	if on, err := p.client.GetEpoch(ctx, onchainID); err == nil && on.AssertionID != (common.Hash{}) {
		args = append(args, "assertion", on.AssertionID.Hex())
	}
	p.log.Info("published index commitment", args...)
	return nil
}

// ResumePending settles submissions whose receipt was never recorded, typically
// because the process restarted mid-wait. It returns how many are still unresolved,
// so a caller can hold off building a new commitment while one is in flight.
func (p *Publisher) ResumePending(ctx context.Context, chainID uint64) (int, error) {
	pending, err := p.st.PendingSubmissions(ctx, chainID)
	if err != nil {
		return 0, err
	}

	unresolved := 0
	for _, e := range pending {
		if e.SubmissionRef == nil {
			// Cannot happen through this code path; treat as lost rather than guess.
			p.log.Warn("submitted epoch has no reference; marking failed", "epoch", e.ID)
			if err := p.st.SetEpochStatus(ctx, e.ID, store.EpochFailed); err != nil {
				return unresolved, err
			}
			continue
		}
		if p.StaleAfter > 0 && e.SubmittedAt != nil && p.now().Sub(*e.SubmittedAt) > p.StaleAfter {
			p.log.Warn("submission never confirmed; marking failed",
				"epoch", e.ID, "ref", e.SubmissionRef.Hex(), "submitted_at", e.SubmittedAt)
			if err := p.st.SetEpochStatus(ctx, e.ID, store.EpochFailed); err != nil {
				return unresolved, err
			}
			continue
		}
		if err := p.settle(ctx, e.ID, *e.SubmissionRef); err != nil {
			if ctx.Err() != nil {
				return unresolved, err
			}
			p.log.Warn("could not settle pending submission", "epoch", e.ID, "err", err)
			unresolved++
		}
	}
	return unresolved, nil
}

// FinalizeDue settles this publisher's commitments that the chain is ready to settle,
// which returns the bond and makes the coverage claimable (see ClaimDue). It also
// mirrors finalizations and rejections that happened without us. Returns how many
// epochs it finalized.
//
// Publishing is only half of the optimistic loop: a commitment stays challengeable
// until someone calls finalizeIndex, and in oracle mode that call is what settles the
// assertion. Nobody else has a reason to pay that gas for us, so the publisher does it.
//
// In local-arbiter mode an unchallenged epoch is due once its deadline has passed and
// a challenged one waits for the arbiter. In oracle mode both states are settled by
// the same call, which reverts until the assertion expires or the vote is in, so the
// call is simulated first and polling costs no gas.
func (p *Publisher) FinalizeDue(ctx context.Context, chainID uint64) (int, error) {
	epochs, err := p.st.PublishedUnfinalized(ctx, chainID)
	if err != nil {
		return 0, err
	}
	if len(epochs) == 0 {
		return 0, nil
	}
	mode, err := p.client.Mode(ctx)
	if err != nil {
		return 0, err
	}

	done := 0
	for _, e := range epochs {
		onchain, err := p.client.GetEpoch(ctx, *e.OnchainID)
		if err != nil {
			return done, err
		}
		switch onchain.Status {
		case EpochFinalized:
			if err := p.st.SetEpochStatus(ctx, e.ID, store.EpochFinalized); err != nil {
				return done, err
			}
			continue
		case EpochRejected:
			p.log.Warn("commitment was rejected on-chain", "epoch", e.ID, "onchain_epoch", *e.OnchainID)
			if err := p.st.SetEpochStatus(ctx, e.ID, store.EpochRejected); err != nil {
				return done, err
			}
			continue
		case EpochChallenged:
			if !mode.OracleMode() {
				p.log.Warn("commitment is under challenge; waiting for the arbiter",
					"epoch", e.ID, "onchain_epoch", *e.OnchainID, "challenger", onchain.Challenger.Hex())
				continue
			}
			// The oracle settles a disputed assertion once its voters have decided;
			// until then finalizeIndex reverts, which is what the simulation catches.
		case EpochProposed:
			// Block timestamps track wall-clock time within seconds on any live chain,
			// and finalizing early merely reverts, so the local clock is good enough.
			deadline := time.Unix(int64(onchain.ChallengeDeadline), 0).Add(p.FinalizeSlack)
			if p.now().Before(deadline) {
				continue
			}
		default:
			continue
		}

		id := new(big.Int).SetInt64(*e.OnchainID)
		if mode.OracleMode() {
			if err := p.client.Simulate(ctx, p.sub.Sender(), "finalizeIndex", id); err != nil {
				// Not ready: still live, or the dispute is still being voted on.
				continue
			}
		}

		data, err := p.client.ABI().Pack("finalizeIndex", id)
		if err != nil {
			return done, fmt.Errorf("hintreg: pack finalizeIndex: %w", err)
		}
		ref, err := p.sub.Submit(ctx, p.client.Address(), nil, data)
		if err != nil {
			return done, fmt.Errorf("hintreg: submit finalizeIndex: %w", err)
		}
		rcpt, err := p.sub.Wait(ctx, ref)
		if err != nil {
			return done, err
		}
		if rcpt.Status != types.ReceiptStatusSuccessful {
			p.log.Warn("finalizeIndex reverted; will retry", "epoch", e.ID, "tx", rcpt.TxHash.Hex())
			continue
		}

		// Settling a disputed assertion can go either way, so read the verdict back
		// rather than assume it.
		status := store.EpochFinalized
		if onchain.Status == EpochChallenged {
			after, err := p.client.GetEpoch(ctx, *e.OnchainID)
			if err != nil {
				return done, err
			}
			if after.Status == EpochRejected {
				status = store.EpochRejected
			}
		}
		if err := p.st.SetEpochStatus(ctx, e.ID, status); err != nil {
			return done, err
		}
		if status == store.EpochRejected {
			p.log.Warn("commitment settled against us", "epoch", e.ID, "onchain_epoch", *e.OnchainID, "tx", rcpt.TxHash.Hex())
			continue
		}
		done++
		p.log.Info("finalized index commitment",
			"epoch", e.ID, "onchain_epoch", *e.OnchainID, "tx", rcpt.TxHash.Hex())
	}
	return done, nil
}

// coverageClaim is HintRegistry.CoverageClaim laid out for ABI packing.
type coverageClaim struct {
	Key       [32]byte
	FromBlock uint64
	ToBlock   uint64
	Proof     [][32]byte
}

// ClaimDue collects the coverage reward for every finalized commitment that has not
// been claimed yet. This is the transaction that pays the publisher: one
// claimCoverage call per epoch, carrying a proof per asset the registry says is
// worth something today. Assets that would pay nothing are left out, and an epoch
// with nothing to claim is marked claimed without a transaction. Returns how many
// epochs were paid.
func (p *Publisher) ClaimDue(ctx context.Context, chainID uint64) (int, error) {
	epochs, err := p.st.UnclaimedFinalized(ctx, chainID)
	if err != nil {
		return 0, err
	}

	paid := 0
	for _, e := range epochs {
		coverage, err := p.st.EpochCoverage(ctx, e.ID)
		if err != nil {
			return paid, err
		}
		leaves := make([]common.Hash, len(coverage))
		for i, c := range coverage {
			leaves[i] = c.Leaf
		}
		tree := merkle.Build(leaves)

		var claims []coverageClaim
		quoted := new(big.Int)
		for i, c := range coverage {
			amt, err := p.client.Claimable(ctx, c.RegistryKey, c.FromBlock, c.ToBlock)
			if err != nil {
				return paid, fmt.Errorf("hintreg: quote coverage for %s: %w", c.Asset.Hex(), err)
			}
			if amt.Sign() == 0 {
				continue
			}
			proof, err := tree.Proof(i)
			if err != nil {
				return paid, err
			}
			cl := coverageClaim{Key: c.RegistryKey, FromBlock: c.FromBlock, ToBlock: c.ToBlock}
			for _, h := range proof {
				cl.Proof = append(cl.Proof, h)
			}
			claims = append(claims, cl)
			quoted.Add(quoted, amt)
		}

		if len(claims) == 0 {
			p.log.Info("nothing claimable for finalized commitment", "epoch", e.ID, "onchain_epoch", *e.OnchainID)
			if err := p.st.MarkClaimed(ctx, e.ID, common.Hash{}, "0"); err != nil {
				return paid, err
			}
			continue
		}

		data, err := p.client.ABI().Pack("claimCoverage", new(big.Int).SetInt64(*e.OnchainID), claims)
		if err != nil {
			return paid, fmt.Errorf("hintreg: pack claimCoverage: %w", err)
		}
		ref, err := p.sub.Submit(ctx, p.client.Address(), nil, data)
		if err != nil {
			return paid, fmt.Errorf("hintreg: submit claimCoverage: %w", err)
		}
		rcpt, err := p.sub.Wait(ctx, ref)
		if err != nil {
			return paid, err
		}
		if rcpt.Status != types.ReceiptStatusSuccessful {
			p.log.Warn("claimCoverage reverted; will retry", "epoch", e.ID, "tx", rcpt.TxHash.Hex())
			continue
		}
		reward := p.rewardFromReceipt(rcpt)
		if err := p.st.MarkClaimed(ctx, e.ID, rcpt.TxHash, reward.String()); err != nil {
			return paid, err
		}
		paid++
		p.log.Info("claimed coverage reward", "epoch", e.ID, "onchain_epoch", *e.OnchainID,
			"assets", len(claims), "quoted_wei", quoted, "paid_wei", reward, "tx", rcpt.TxHash.Hex())
	}
	return paid, nil
}

// rewardFromReceipt sums the CoverageRewarded logs our registry emitted.
func (p *Publisher) rewardFromReceipt(r *types.Receipt) *big.Int {
	total := new(big.Int)
	ev, ok := p.client.ABI().Events["CoverageRewarded"]
	if !ok {
		return total
	}
	for _, l := range r.Logs {
		if l.Address != p.client.Address() || len(l.Topics) == 0 || l.Topics[0] != ev.ID {
			continue
		}
		vals, err := ev.Inputs.NonIndexed().Unpack(l.Data)
		if err != nil || len(vals) != 4 {
			continue
		}
		if reward, ok := vals[3].(*big.Int); ok {
			total.Add(total, reward)
		}
	}
	return total
}

// epochIDFromReceipt reads the registry's epoch id out of the IndexPublished log.
func (p *Publisher) epochIDFromReceipt(r *types.Receipt) (int64, error) {
	ev, ok := p.client.ABI().Events["IndexPublished"]
	if !ok {
		return 0, errors.New("hintreg: ABI has no IndexPublished event")
	}
	for _, l := range r.Logs {
		if l.Address != p.client.Address() || len(l.Topics) < 2 || l.Topics[0] != ev.ID {
			continue
		}
		// epochId is the first indexed parameter.
		return new(big.Int).SetBytes(l.Topics[1].Bytes()).Int64(), nil
	}
	return 0, errors.New("hintreg: IndexPublished log not found in receipt")
}

// ProofFor rebuilds a commitment's tree and returns an account's inclusion proof.
func ProofFor(ctx context.Context, st *store.Store, epochID int64, account common.Address) (leaf store.EpochLeaf, proof []common.Hash, err error) {
	leaf, err = st.EpochLeafFor(ctx, epochID, account)
	if err != nil {
		return store.EpochLeaf{}, nil, err
	}
	rows, err := st.EpochLeaves(ctx, epochID)
	if err != nil {
		return store.EpochLeaf{}, nil, err
	}

	leaves := make([]common.Hash, len(rows))
	for i, r := range rows {
		leaves[i] = r.Leaf
	}
	tree := merkle.Build(leaves)
	proof, err = tree.Proof(leaf.Index)
	if err != nil {
		return store.EpochLeaf{}, nil, err
	}
	return leaf, proof, nil
}

// checkTrusted refuses to build for a chain the deployment does not vouch for.
//
// A chain with no profile row at all is treated as trusted, which sounds
// backwards and is not: profiles are written when a chain starts, so the only way
// to reach this without one is a database that predates migration 0006. Refusing
// there would silently stop a working deployment from publishing after an
// upgrade, which is a worse failure than the one this guards against — and every
// chain that deployment runs came from its own config file.
func (p *Publisher) checkTrusted(ctx context.Context, chainID uint64) error {
	prof, err := p.st.GetChainProfile(ctx, chainID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if prof.Trust == store.TrustVerified {
		return nil
	}
	return fmt.Errorf("%w: chain %d is %s", ErrUntrusted, chainID, prof.Trust)
}
