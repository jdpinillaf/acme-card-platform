package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

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
// balances, with their hash.
func (s *Service) snapshotOne(ctx context.Context, accountID string) error {
	acc, err := s.load(ctx, s.pool, accountID, false)
	if err != nil {
		return err
	}
	b, err := s.ledger.GetBalances(ctx, &ledgerv1.GetBalancesRequest{AccountId: acc.ID})
	if err != nil {
		return err
	}
	balances := map[string]int64{}
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
	return outbox.Enqueue(ctx, s.pool, ev)
}

// SnapshotAccounts emits snapshot.account now for the given accounts, or every non-cancelled
// account when none is given.
func (s *Service) SnapshotAccounts(ctx context.Context, req *accountsv1.SnapshotAccountsRequest) (*accountsv1.SnapshotAccountsResponse, error) {
	ids := req.AccountIds
	if len(ids) == 0 {
		rows, err := s.pool.Query(ctx, `SELECT id::text FROM accounts WHERE state <> 'cancelled' ORDER BY id`)
		if err != nil {
			return nil, internal(err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	n := 0
	for _, id := range ids {
		if err := s.snapshotOne(ctx, id); err != nil {
			return nil, err
		}
		n++
	}
	return &accountsv1.SnapshotAccountsResponse{Count: int32(n)}, nil
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
