package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/money"
	"github.com/acme/acme-card/pkg/outbox"
)

// Service implements ledgerv1.LedgerServiceServer on Postgres.
type Service struct {
	ledgerv1.UnimplementedLedgerServiceServer
	pool *pgxpool.Pool
}

// NewService builds the ledger service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// OpenLedger creates the sub-accounts and zero balances. Idempotent for the same currency.
func (s *Service) OpenLedger(ctx context.Context, req *ledgerv1.OpenLedgerRequest) (*ledgerv1.OpenLedgerResponse, error) {
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "account_id must be a uuid")
	}
	if !money.ValidCurrency(req.Currency) {
		return nil, status.Error(codes.InvalidArgument, "invalid currency")
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, name := range subAccountNames {
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (account_id, sub_account, currency) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, req.AccountId, name, req.Currency); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO balances (account_id, sub_account) VALUES ($1,$2) ON CONFLICT DO NOTHING`, req.AccountId, name); err != nil {
				return err
			}
		}
		var mismatched int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts WHERE account_id=$1 AND currency<>$2`, req.AccountId, req.Currency).Scan(&mismatched); err != nil {
			return err
		}
		if mismatched > 0 {
			return status.Error(codes.FailedPrecondition, "ledger already opened in another currency")
		}
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.OpenLedgerResponse{}, nil
}

// PostJournal atomically records a balanced journal. Insert-first on the unique reference makes
// concurrent duplicates wait for the winner and then replay its entry.
func (s *Service) PostJournal(ctx context.Context, req *ledgerv1.PostJournalRequest) (*ledgerv1.PostJournalResponse, error) {
	if req.Reference == "" || req.EntryType == "" {
		return nil, status.Error(codes.InvalidArgument, "reference and entry_type are required")
	}
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "account_id must be a uuid")
	}
	if err := ValidatePostings(req.Postings); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	hash := PostingsHash(req.EntryType, req.Postings)
	var resp *ledgerv1.PostJournalResponse

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var opened int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts WHERE account_id=$1`, req.AccountId).Scan(&opened); err != nil {
			return err
		}
		if opened == 0 {
			return status.Error(codes.NotFound, "ledger not opened for account")
		}
		id := uuid.Must(uuid.NewV7())
		var keyHash, actor *string
		if m := req.Meta; m != nil {
			if m.IdempotencyKeyHash != "" {
				keyHash = &m.IdempotencyKeyHash
			}
			if m.Actor != "" {
				actor = &m.Actor
			}
		}
		var postedAt time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO journal_entries (id, account_id, reference, entry_type, postings_hash, idempotency_key_hash, actor)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (reference) DO NOTHING
			RETURNING posted_at`, id, req.AccountId, req.Reference, req.EntryType, hash, keyHash, actor).Scan(&postedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// Someone else owns this reference (we waited for them to commit). Replay.
			var existingID uuid.UUID
			var existingHash, existingAcc string
			if err := tx.QueryRow(ctx, `SELECT id, postings_hash, account_id::text, posted_at FROM journal_entries WHERE reference=$1`, req.Reference).
				Scan(&existingID, &existingHash, &existingAcc, &postedAt); err != nil {
				return err
			}
			if existingHash != hash || existingAcc != req.AccountId {
				return status.Error(codes.AlreadyExists, "reference already used for a different journal")
			}
			resp = &ledgerv1.PostJournalResponse{JournalEntryId: existingID.String(), Replayed: true, PostedAt: timestamppb.New(postedAt)}
			return nil
		}
		if err != nil {
			return err
		}

		// Aggregate deltas per sub-account and lock balance rows in a fixed order (no deadlocks).
		deltas := map[string]int64{}
		for _, p := range req.Postings {
			name := subAccountNames[p.SubAccount]
			if _, err := tx.Exec(ctx, `INSERT INTO postings (journal_entry_id, account_id, sub_account, side, amount_minor) VALUES ($1,$2,$3,$4,$5)`,
				id, req.AccountId, name, sideName(p.Side), p.AmountMinor); err != nil {
				return err
			}
			if p.Side == ledgerv1.Side_SIDE_DEBIT {
				deltas[name] += p.AmountMinor
			} else {
				deltas[name] -= p.AmountMinor
			}
		}
		names := make([]string, 0, len(deltas))
		for n := range deltas {
			names = append(names, n)
		}
		sort.Strings(names)
		after := map[string]int64{}
		for _, n := range names {
			var bal int64
			if err := tx.QueryRow(ctx, `UPDATE balances SET raw_balance_minor = raw_balance_minor + $3, updated_at = now()
				WHERE account_id=$1 AND sub_account=$2 RETURNING raw_balance_minor`, req.AccountId, n, deltas[n]).Scan(&bal); err != nil {
				return err
			}
			after[n] = NormalBalance(subAccountFromName(n), bal)
		}

		ev := events.New("ledger.journal_posted", "journal_entry", id.String())
		if actor != nil {
			ev.Actor = *actor
		}
		if keyHash != nil {
			ev.IdempotencyKeyHash = *keyHash
		}
		ev.After, _ = json.Marshal(map[string]any{"account_id": req.AccountId, "reference": req.Reference, "entry_type": req.EntryType, "balances": after})
		if err := outbox.Enqueue(ctx, tx, ev); err != nil {
			return err
		}
		resp = &ledgerv1.PostJournalResponse{JournalEntryId: id.String(), PostedAt: timestamppb.New(postedAt)}
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

// GetBalances returns natural-sign balances for every sub-account.
func (s *Service) GetBalances(ctx context.Context, req *ledgerv1.GetBalancesRequest) (*ledgerv1.GetBalancesResponse, error) {
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "account_id must be a uuid")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT la.currency, b.sub_account, b.raw_balance_minor
		FROM balances b JOIN ledger_accounts la USING (account_id, sub_account)
		WHERE b.account_id = $1 ORDER BY b.sub_account`, req.AccountId)
	if err != nil {
		return nil, toStatus(err)
	}
	defer rows.Close()
	resp := &ledgerv1.GetBalancesResponse{}
	for rows.Next() {
		var name string
		var raw int64
		if err := rows.Scan(&resp.Currency, &name, &raw); err != nil {
			return nil, toStatus(err)
		}
		sa := subAccountFromName(name)
		resp.Balances = append(resp.Balances, &ledgerv1.Balance{SubAccount: sa, AmountMinor: NormalBalance(sa, raw)})
	}
	if err := rows.Err(); err != nil {
		return nil, toStatus(err)
	}
	if len(resp.Balances) == 0 {
		return nil, status.Error(codes.NotFound, "ledger not opened for account")
	}
	return resp, nil
}

func sideName(s ledgerv1.Side) string {
	if s == ledgerv1.Side_SIDE_DEBIT {
		return "debit"
	}
	return "credit"
}

func toStatus(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
