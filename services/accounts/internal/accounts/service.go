package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/money"
	"github.com/acme/acme-card/pkg/outbox"
)

const uniqueViolation = "23505"

// Service implements accountsv1.AccountsServiceServer.
type Service struct {
	accountsv1.UnimplementedAccountsServiceServer
	pool   *pgxpool.Pool
	ledger ledgerv1.LedgerServiceClient
}

// NewService builds the accounts service.
func NewService(pool *pgxpool.Pool, ledger ledgerv1.LedgerServiceClient) *Service {
	return &Service{pool: pool, ledger: ledger}
}

type accountRow struct {
	ID            string    `json:"id"`
	CustomerID    string    `json:"customer_id"`
	Currency      string    `json:"currency"`
	State         State     `json:"state"`
	Frozen        bool      `json:"frozen"`
	Collateral    int64     `json:"collateral_minor"`
	CreditLimit   int64     `json:"credit_limit_minor"`
	PurchaseLimit int64     `json:"purchase_limit_minor"`
	CreatedAt     time.Time `json:"-"`
	UpdatedAt     time.Time `json:"-"`
}

const accountCols = `id::text, customer_id, currency, state, frozen, collateral_minor, credit_limit_minor, purchase_limit_minor, created_at, updated_at`

func scanAccount(row pgx.Row) (accountRow, error) {
	var a accountRow
	err := row.Scan(&a.ID, &a.CustomerID, &a.Currency, &a.State, &a.Frozen, &a.Collateral, &a.CreditLimit, &a.PurchaseLimit, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (a accountRow) proto(available int64) *accountsv1.Account {
	return &accountsv1.Account{
		Id: a.ID, CustomerId: a.CustomerID, Currency: a.Currency, State: stateProto(a.State), Frozen: a.Frozen,
		CreditLimitMinor: a.CreditLimit, AvailableCreditMinor: available, CollateralMinor: a.Collateral,
		PurchaseLimitMinor: a.PurchaseLimit, CreatedAt: timestamppb.New(a.CreatedAt), UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}

func snapshot(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func event(typ, aggType, aggID string, meta *commonv1.RequestMeta) events.Event {
	ev := events.New(typ, aggType, aggID)
	if meta != nil {
		ev.Actor, ev.IdempotencyKeyHash = meta.Actor, meta.IdempotencyKeyHash
	}
	return ev
}

// CreateAccount opens an account + card in pending_collateral and its ledger.
func (s *Service) CreateAccount(ctx context.Context, req *accountsv1.CreateAccountRequest) (*accountsv1.CreateAccountResponse, error) {
	if req.CustomerId == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_customer", "customer_id is required")
	}
	if !money.ValidCurrency(req.Currency) {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_currency", "currency must be an ISO-4217 code")
	}
	id := uuid.Must(uuid.NewV7()).String()
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		acc, err = scanAccount(tx.QueryRow(ctx, `INSERT INTO accounts (id, customer_id, currency, state) VALUES ($1,$2,$3,$4) RETURNING `+accountCols,
			id, req.CustomerId, req.Currency, StatePendingCollateral))
		if err != nil {
			return err
		}
		ev := event("account.opened", "account", id, req.Meta)
		ev.After = snapshot(acc)
		return outbox.Enqueue(ctx, tx, ev)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return nil, grpcx.Error(codes.AlreadyExists, "account_exists", "customer already has an account in this currency")
	}
	if err != nil {
		return nil, internal(err)
	}
	// Ledger opening is idempotent; settlement re-ensures it, so a failure here is recoverable.
	if _, err := s.ledger.OpenLedger(ctx, &ledgerv1.OpenLedgerRequest{AccountId: id, Currency: req.Currency}); err != nil {
		return nil, err
	}
	return &accountsv1.CreateAccountResponse{Account: acc.proto(0)}, nil
}

// GetAccount returns the account; available credit is derived from the ledger.
func (s *Service) GetAccount(ctx context.Context, req *accountsv1.GetAccountRequest) (*accountsv1.GetAccountResponse, error) {
	acc, err := s.load(ctx, s.pool, req.AccountId, false)
	if err != nil {
		return nil, err
	}
	avail, err := s.available(ctx, acc.ID)
	if err != nil {
		return nil, err
	}
	return &accountsv1.GetAccountResponse{Account: acc.proto(avail)}, nil
}

// CreateDeposit registers a pending collateral deposit intent in the account currency.
func (s *Service) CreateDeposit(ctx context.Context, req *accountsv1.CreateDepositRequest) (*accountsv1.CreateDepositResponse, error) {
	if req.AmountMinor <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_amount", "amount_minor must be positive")
	}
	var dep *accountsv1.Deposit
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		acc, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		if err := CanAcceptDeposit(acc.State); err != nil {
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		}
		id := uuid.Must(uuid.NewV7()).String()
		var created time.Time
		if err := tx.QueryRow(ctx, `INSERT INTO deposits (id, account_id, amount_minor, currency, status) VALUES ($1,$2,$3,$4,'pending') RETURNING created_at`,
			id, acc.ID, req.AmountMinor, acc.Currency).Scan(&created); err != nil {
			return err
		}
		dep = &accountsv1.Deposit{Id: id, AccountId: acc.ID, Amount: &commonv1.Money{AmountMinor: req.AmountMinor, Currency: acc.Currency},
			Status: accountsv1.DepositStatus_DEPOSIT_STATUS_PENDING, CreatedAt: timestamppb.New(created)}
		ev := event("collateral.deposit_created", "deposit", id, req.Meta)
		ev.After = snapshot(map[string]any{"account_id": acc.ID, "amount_minor": req.AmountMinor, "currency": acc.Currency, "status": "pending"})
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return &accountsv1.CreateDepositResponse{Deposit: dep}, nil
}

type depositRow struct {
	ID, AccountID, Currency, Status string
	Amount                          int64
	ExternalRef                     *string
	CreatedAt                       time.Time
	SettledAt                       *time.Time
}

func (d depositRow) proto() *accountsv1.Deposit {
	p := &accountsv1.Deposit{Id: d.ID, AccountId: d.AccountID, Amount: &commonv1.Money{AmountMinor: d.Amount, Currency: d.Currency},
		Status: depositStatusProto(d.Status), CreatedAt: timestamppb.New(d.CreatedAt)}
	if d.ExternalRef != nil {
		p.ExternalRef = *d.ExternalRef
	}
	if d.SettledAt != nil {
		p.SettledAt = timestamppb.New(*d.SettledAt)
	}
	return p
}

func (s *Service) loadDeposit(ctx context.Context, q pgx.Tx, id string) (depositRow, error) {
	var d depositRow
	err := q.QueryRow(ctx, `SELECT id::text, account_id::text, currency, status, amount_minor, external_ref, created_at, settled_at FROM deposits WHERE id=$1 FOR UPDATE`, id).
		Scan(&d.ID, &d.AccountID, &d.Currency, &d.Status, &d.Amount, &d.ExternalRef, &d.CreatedAt, &d.SettledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, grpcx.Error(codes.NotFound, "deposit_not_found", "deposit not found")
	}
	return d, err
}

// SettleDeposit applies the bank's settlement. Flow: validate → post ledger journal (idempotent
// on the deposit id) → lock deposit+account, apply limit/state, emit events. Concurrent or
// repeated webhooks replay; a crash between ledger and accounts is healed by the next retry.
func (s *Service) SettleDeposit(ctx context.Context, req *accountsv1.SettleDepositRequest) (*accountsv1.SettleDepositResponse, error) {
	if _, err := uuid.Parse(req.DepositId); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_deposit", "deposit_id must be a uuid")
	}
	if req.ExternalRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_external_ref", "external_ref is required")
	}
	if req.Status != accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED && req.Status != accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_status", "status must be settled or failed")
	}

	// Pre-check outside the lock so the ledger call is only made for legitimate settlements.
	var dep depositRow
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if dep, err = s.loadDeposit(ctx, tx, req.DepositId); err != nil {
			return err
		}
		acc, err = s.load(ctx, tx, dep.AccountID, false)
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	if resp, done, err := s.terminal(ctx, dep, acc, req); done {
		return resp, err
	}
	if req.AmountMinor != dep.Amount {
		return nil, grpcx.Error(codes.InvalidArgument, "amount_mismatch", "settled amount differs from deposit amount")
	}
	if req.Currency != dep.Currency {
		return nil, grpcx.Error(codes.InvalidArgument, "currency_mismatch", "settled currency differs from deposit currency")
	}

	var journalID string
	if req.Status == accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED {
		if _, err := s.ledger.OpenLedger(ctx, &ledgerv1.OpenLedgerRequest{AccountId: acc.ID, Currency: acc.Currency}); err != nil {
			return nil, err
		}
		jr, err := s.ledger.PostJournal(ctx, &ledgerv1.PostJournalRequest{
			AccountId: acc.ID, Reference: "deposit:" + dep.ID, EntryType: "collateral_deposit_settled", Meta: req.Meta,
			Postings: []*ledgerv1.Posting{
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: dep.Amount},
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD, Side: ledgerv1.Side_SIDE_CREDIT, AmountMinor: dep.Amount},
			},
		})
		if err != nil {
			return nil, err
		}
		journalID = jr.JournalEntryId
	}

	var resp *accountsv1.SettleDepositResponse
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if dep, err = s.loadDeposit(ctx, tx, req.DepositId); err != nil {
			return err
		}
		if acc, err = s.load(ctx, tx, dep.AccountID, true); err != nil {
			return err
		}
		if r, done, err := s.terminal(ctx, dep, acc, req); done {
			resp = r
			return err
		}
		if req.Status == accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED {
			if err := tx.QueryRow(ctx, `UPDATE deposits SET status='failed', external_ref=$2, settled_at=now() WHERE id=$1 RETURNING settled_at`, dep.ID, req.ExternalRef).Scan(&dep.SettledAt); err != nil {
				return err
			}
			dep.Status, dep.ExternalRef = "failed", &req.ExternalRef
			ev := event("collateral.deposit_failed", "deposit", dep.ID, req.Meta)
			ev.After = snapshot(map[string]any{"account_id": acc.ID, "external_ref": req.ExternalRef, "status": "failed"})
			if err := outbox.Enqueue(ctx, tx, ev); err != nil {
				return err
			}
			resp = &accountsv1.SettleDepositResponse{Deposit: dep.proto(), Account: acc.proto(acc.CreditLimit)}
			return nil
		}

		next, err := Transition(acc.State, EventCollateralConfirmed)
		if err != nil {
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		}
		before := acc
		if err := tx.QueryRow(ctx, `UPDATE deposits SET status='settled', external_ref=$2, journal_entry_id=$3, settled_at=now() WHERE id=$1 RETURNING settled_at`,
			dep.ID, req.ExternalRef, journalID).Scan(&dep.SettledAt); err != nil {
			return err
		}
		dep.Status, dep.ExternalRef = "settled", &req.ExternalRef
		acc, err = scanAccount(tx.QueryRow(ctx, `
			UPDATE accounts SET collateral_minor = collateral_minor + $2, credit_limit_minor = collateral_minor + $2,
			       state = $3, version = version + 1, updated_at = now()
			WHERE id = $1 RETURNING `+accountCols, acc.ID, dep.Amount, next))
		if err != nil {
			return err
		}
		confirmed := event("collateral.confirmed", "deposit", dep.ID, req.Meta)
		confirmed.Before = snapshot(before)
		confirmed.After = snapshot(map[string]any{"account": acc, "deposit_id": dep.ID, "external_ref": req.ExternalRef, "journal_entry_id": journalID})
		evs := []events.Event{confirmed}
		if before.State != acc.State {
			activated := event("card.activated", "account", acc.ID, req.Meta)
			activated.Before, activated.After = snapshot(before), snapshot(acc)
			evs = append(evs, activated)
		}
		if err := outbox.Enqueue(ctx, tx, evs...); err != nil {
			return err
		}
		resp = &accountsv1.SettleDepositResponse{Deposit: dep.proto(), Account: acc.proto(0)}
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return nil, grpcx.Error(codes.AlreadyExists, "external_ref_reused", "external_ref already used by another deposit")
	}
	if err != nil {
		return nil, internal(err)
	}
	if resp.Account.AvailableCreditMinor == 0 && !resp.Replayed && resp.Deposit.Status == accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED {
		if resp.Account.AvailableCreditMinor, err = s.available(ctx, acc.ID); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// terminal handles deposits that are no longer pending (replay or conflict) and closed accounts.
func (s *Service) terminal(ctx context.Context, dep depositRow, acc accountRow, req *accountsv1.SettleDepositRequest) (*accountsv1.SettleDepositResponse, bool, error) {
	if dep.Status != "pending" {
		same := dep.ExternalRef != nil && *dep.ExternalRef == req.ExternalRef && depositStatusProto(dep.Status) == req.Status
		if !same {
			if dep.Status == "failed" {
				return nil, true, grpcx.Error(codes.FailedPrecondition, "deposit_failed", "deposit already failed")
			}
			return nil, true, grpcx.Error(codes.AlreadyExists, "deposit_already_settled", "deposit already settled with a different external_ref")
		}
		avail, err := s.available(ctx, acc.ID)
		if err != nil {
			return nil, true, err
		}
		return &accountsv1.SettleDepositResponse{Deposit: dep.proto(), Account: acc.proto(avail), Replayed: true}, true, nil
	}
	if acc.State == StateClosed {
		return nil, true, grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
	}
	return nil, false, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Service) load(ctx context.Context, q querier, id string, lock bool) (accountRow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return accountRow{}, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	sql := `SELECT ` + accountCols + ` FROM accounts WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	acc, err := scanAccount(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return acc, grpcx.Error(codes.NotFound, "account_not_found", "account not found")
	}
	return acc, err
}

// available = collateral_held − receivable, straight from the ledger (source of truth).
func (s *Service) available(ctx context.Context, accountID string) (int64, error) {
	b, err := s.ledger.GetBalances(ctx, &ledgerv1.GetBalancesRequest{AccountId: accountID})
	if status.Code(err) == codes.NotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var coll, recv int64
	for _, x := range b.Balances {
		switch x.SubAccount {
		case ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD:
			coll = x.AmountMinor
		case ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE:
			recv = x.AmountMinor
		}
	}
	return max(coll-recv, 0), nil
}

func stateProto(s State) accountsv1.AccountState {
	switch s {
	case StatePendingCollateral:
		return accountsv1.AccountState_ACCOUNT_STATE_PENDING_COLLATERAL
	case StateActive:
		return accountsv1.AccountState_ACCOUNT_STATE_ACTIVE
	case StateClosed:
		return accountsv1.AccountState_ACCOUNT_STATE_CLOSED
	}
	return accountsv1.AccountState_ACCOUNT_STATE_UNSPECIFIED
}

func depositStatusProto(s string) accountsv1.DepositStatus {
	switch s {
	case "pending":
		return accountsv1.DepositStatus_DEPOSIT_STATUS_PENDING
	case "settled":
		return accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED
	case "failed":
		return accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED
	}
	return accountsv1.DepositStatus_DEPOSIT_STATUS_UNSPECIFIED
}

func internal(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
