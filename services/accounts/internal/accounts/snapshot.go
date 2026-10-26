package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/outbox"
)

// normalizeJSON re-encodes a JSON value with sorted object keys and exact numbers, so a hash of
// it survives a round trip through jsonb.
func normalizeJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// SnapshotHash is sha256 over the normalized {account, balances} of a snapshot.account event.
func SnapshotHash(account json.RawMessage, balances map[string]int64) (string, error) {
	acc, err := normalizeJSON(account)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Account  json.RawMessage  `json:"account"`
		Balances map[string]int64 `json:"balances"`
	}{acc, balances})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

var subAccountKey = map[ledgerv1.SubAccount]string{
	ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING:    "cash_clearing",
	ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD:  "collateral_held",
	ledgerv1.SubAccount_SUB_ACCOUNT_CREDIT_AVAILABLE: "credit_available",
	ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE:       "receivable",
	ledgerv1.SubAccount_SUB_ACCOUNT_INTEREST_INCOME:  "interest_income",
	ledgerv1.SubAccount_SUB_ACCOUNT_FEE_INCOME:       "fee_income",
}

// snapshotOne emits snapshot.account: the account row (masked, as in every event) plus its ledger
// balances, with their hash. The row is locked from the read to the enqueue, so a concurrent
// change publishes its account.changed after this snapshot, never before a stale one.
func (s *Service) snapshotOne(ctx context.Context, accountID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		acc, err := s.load(ctx, tx, accountID, true)
		if err != nil {
			return err
		}
		balances := map[string]int64{}
		b, err := s.ledger.GetBalances(ctx, &ledgerv1.GetBalancesRequest{AccountId: acc.ID})
		switch {
		case status.Code(err) == codes.NotFound:
			b = &ledgerv1.GetBalancesResponse{} // ledger not opened yet (CreateAccount opens it after commit; settlement retries): no balances
		case err != nil:
			return err
		}
		for _, bal := range b.Balances {
			balances[subAccountKey[bal.SubAccount]] = bal.AmountMinor
		}
		accJSON, err := json.Marshal(acc)
		if err != nil {
			return err
		}
		hash, err := SnapshotHash(accJSON, balances)
		if err != nil {
			return err
		}
		ev := event("snapshot.account", "account", acc.ID, &commonv1.RequestMeta{Actor: "accounts.snapshot"})
		ev.After = snapshot(map[string]any{"account": json.RawMessage(accJSON), "balances": balances, "snapshot_hash": hash})
		return outbox.Enqueue(ctx, tx, ev)
	})
}

// SnapshotAccounts emits snapshot.account now for the given accounts, or every non-cancelled
// account when none is given. Background runs the all-accounts snapshot detached from the
// caller (a large book takes longer than an API request may), one run at a time.
func (s *Service) SnapshotAccounts(ctx context.Context, req *accountsv1.SnapshotAccountsRequest) (*accountsv1.SnapshotAccountsResponse, error) {
	if req.Background && len(req.AccountIds) > 0 {
		return nil, status.Error(codes.InvalidArgument, "background snapshots cover every account")
	}
	ids := req.AccountIds
	if len(ids) == 0 {
		var err error
		if ids, err = s.snapshotIDs(ctx); err != nil {
			return nil, internal(err)
		}
	}
	if !req.Background {
		if err := s.snapshotAll(ctx, ids); err != nil {
			return nil, err
		}
		return &accountsv1.SnapshotAccountsResponse{Count: int32(len(ids))}, nil
	}
	if !s.snapshotting.CompareAndSwap(false, true) {
		return nil, status.Error(codes.FailedPrecondition, "a background snapshot is already running")
	}
	go func() {
		defer s.snapshotting.Store(false)
		if err := s.snapshotAll(context.WithoutCancel(ctx), ids); err != nil {
			slog.Error("background account snapshot failed", "err", err)
		}
	}()
	return &accountsv1.SnapshotAccountsResponse{Count: int32(len(ids)), Background: true}, nil
}

func (s *Service) snapshotIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM accounts WHERE state <> 'cancelled' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// snapshotAll snapshots ids with a few workers. A failing account does not stop the run: the
// others are still snapshotted and the failures are reported together.
func (s *Service) snapshotAll(ctx context.Context, ids []string) error {
	work := make(chan string)
	var mu sync.Mutex
	var failed int
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				if err := s.snapshotOne(ctx, id); err != nil {
					mu.Lock()
					if failed++; firstErr == nil {
						firstErr = fmt.Errorf("account %s: %w", id, err)
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		work <- id
	}
	close(work)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		return status.Errorf(codes.Unavailable, "%d of %d account snapshots failed (first: %v)", failed, len(ids), firstErr)
	}
	return nil
}

// SnapshotDaily snapshots every account once per UTC day (the first caller of the day wins).
func (s *Service) SnapshotDaily(ctx context.Context) (int, error) {
	day := s.clock.Now().UTC().Format("2006-01-02")
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO snapshot_runs (day) VALUES ($1) ON CONFLICT DO NOTHING`, day)
		claimed = tag.RowsAffected() == 1
		return err
	})
	if err != nil || !claimed {
		return 0, internal0(err)
	}
	r, err := s.SnapshotAccounts(ctx, &accountsv1.SnapshotAccountsRequest{})
	if err != nil {
		return 0, err
	}
	return int(r.Count), nil
}

// RunSnapshots checks every interval whether today's snapshot run is due.
func (s *Service) RunSnapshots(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.SnapshotDaily(ctx); err != nil && ctx.Err() == nil {
			log.Error("daily snapshot failed", "err", err)
		} else if n > 0 {
			log.Info("daily account snapshots emitted", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
