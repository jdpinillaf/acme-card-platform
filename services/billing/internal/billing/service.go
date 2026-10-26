// Package billing implements the billing service (Sprint 4, ADR-019): monthly statements in S3,
// interest, late fees, delinquency stages and charge-off, driven by each account's simulated time.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/services/billing/internal/engine"
)

// Config holds billing's pricing.
type Config struct {
	APRBps int64            // annual rate in basis points (2280 = 22.80% = 1.9% monthly)
	Fixed  map[string]int64 // per-currency minimum-payment floor and late fee, minor units
	Bucket string           // S3 bucket holding statement documents
}

// ParseFixedAmounts reads "USD:2500,EUR:2500" into a currency → minor-units map.
func ParseFixedAmounts(s string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		cur, amt, ok := strings.Cut(part, ":")
		n, err := strconv.ParseInt(amt, 10, 64)
		if !ok || len(cur) != 3 || err != nil || n < 0 {
			return nil, fmt.Errorf("billing: bad fixed amount %q", part)
		}
		out[cur] = n
	}
	return out, nil
}

// Service implements billingv1.BillingServiceServer.
type Service struct {
	billingv1.UnimplementedBillingServiceServer
	pool     *pgxpool.Pool
	ledger   ledgerv1.LedgerServiceClient
	accounts accountsv1.AccountsServiceClient
	store    ObjectStore
	cfg      Config
}

// NewService builds the billing service.
func NewService(pool *pgxpool.Pool, ledger ledgerv1.LedgerServiceClient, accounts accountsv1.AccountsServiceClient, store ObjectStore, cfg Config) *Service {
	return &Service{pool: pool, ledger: ledger, accounts: accounts, store: store, cfg: cfg}
}

const statementCols = `id::text, account_id::text, cycle, currency, period_start, period_end, due_date, previous_balance_minor, purchases_minor,
	refunds_minor, payments_minor, returns_minor, interest_minor, fees_minor, waivers_minor, collateral_applied_minor, new_balance_minor,
	minimum_due_minor, past_due_minor, principal_minor, interest_bucket_minor, fee_bucket_minor, grace, balance_only, s3_key, sha256, generated_at, final`

type statementRow struct {
	ID, AccountID, Currency, S3Key, SHA256 string
	Cycle                                  int
	PeriodStart, PeriodEnd, GeneratedAt    time.Time
	DueDate                                *time.Time
	Previous, Purchases, Refunds, Payments int64
	Returns, Interest, Fees, Waivers       int64
	Collateral, NewBalance, Minimum        int64
	PastDue, Principal, IBucket, FBucket   int64
	Grace, BalanceOnly, Final              bool
}

func scanStatement(row pgx.Row) (statementRow, error) {
	var s statementRow
	err := row.Scan(&s.ID, &s.AccountID, &s.Cycle, &s.Currency, &s.PeriodStart, &s.PeriodEnd, &s.DueDate, &s.Previous, &s.Purchases,
		&s.Refunds, &s.Payments, &s.Returns, &s.Interest, &s.Fees, &s.Waivers, &s.Collateral, &s.NewBalance,
		&s.Minimum, &s.PastDue, &s.Principal, &s.IBucket, &s.FBucket, &s.Grace, &s.BalanceOnly, &s.S3Key, &s.SHA256, &s.GeneratedAt, &s.Final)
	return s, err
}

func (s statementRow) facts() engine.StatementFacts {
	f := engine.StatementFacts{Cycle: s.Cycle, Close: s.PeriodEnd, Minimum: s.Minimum, NewBalance: s.NewBalance, BalanceOnly: s.BalanceOnly}
	if s.DueDate != nil {
		f.Due = *s.DueDate
	}
	return f
}

func tsp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func (s statementRow) proto() *billingv1.Statement {
	return &billingv1.Statement{Id: s.ID, AccountId: s.AccountID, Cycle: int32(s.Cycle), Currency: s.Currency,
		PeriodStart: timestamppb.New(s.PeriodStart), PeriodEnd: timestamppb.New(s.PeriodEnd), DueDate: tsp(s.DueDate),
		PreviousBalanceMinor: s.Previous, PurchasesMinor: s.Purchases, RefundsMinor: s.Refunds, PaymentsMinor: s.Payments,
		ReturnsMinor: s.Returns, InterestMinor: s.Interest, FeesMinor: s.Fees, WaiversMinor: s.Waivers, CollateralAppliedMinor: s.Collateral,
		NewBalanceMinor: s.NewBalance, MinimumDueMinor: s.Minimum, PrincipalMinor: s.Principal, InterestBucketMinor: s.IBucket,
		FeeBucketMinor: s.FBucket, S3Key: s.S3Key, Sha256: s.SHA256, GeneratedAt: timestamppb.New(s.GeneratedAt), Final: s.Final}
}

func (s *Service) loadStatements(ctx context.Context, accountID string) ([]statementRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+statementCols+` FROM statements WHERE account_id=$1 ORDER BY cycle`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []statementRow
	for rows.Next() {
		st, err := scanStatement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func validAccount(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	return nil
}

// ListStatements returns the account's statements, oldest first.
func (s *Service) ListStatements(ctx context.Context, req *billingv1.ListStatementsRequest) (*billingv1.ListStatementsResponse, error) {
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	sts, err := s.loadStatements(ctx, req.AccountId)
	if err != nil {
		return nil, internal(err)
	}
	resp := &billingv1.ListStatementsResponse{}
	for _, st := range sts {
		resp.Statements = append(resp.Statements, st.proto())
	}
	return resp, nil
}

// GetStatement returns one statement with its S3 document.
func (s *Service) GetStatement(ctx context.Context, req *billingv1.GetStatementRequest) (*billingv1.GetStatementResponse, error) {
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.StatementId); err != nil {
		return nil, grpcx.Error(codes.NotFound, "statement_not_found", "statement not found")
	}
	st, err := scanStatement(s.pool.QueryRow(ctx, `SELECT `+statementCols+` FROM statements WHERE id=$1 AND account_id=$2`, req.StatementId, req.AccountId))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, grpcx.Error(codes.NotFound, "statement_not_found", "statement not found")
	}
	if err != nil {
		return nil, internal(err)
	}
	body, err := s.store.Get(ctx, st.S3Key)
	if err != nil {
		return nil, internal(err)
	}
	// A statement download is a sensitive read: audited explicitly (ADR-021).
	read := event("statement.read", "statement", st.ID, map[string]any{"account_id": st.AccountID, "cycle": st.Cycle})
	if actor := req.GetMeta().GetActor(); actor != "" {
		read.Actor = actor // who downloaded it; "billing" only for internal reads
	}
	if err := outbox.Enqueue(ctx, s.pool, read); err != nil {
		return nil, internal(err)
	}
	return &billingv1.GetStatementResponse{Statement: st.proto(), BodyJson: body}, nil
}

// ListCharges returns interest and fee charges, oldest first.
func (s *Service) ListCharges(ctx context.Context, req *billingv1.ListChargesRequest) (*billingv1.ListChargesResponse, error) {
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+chargeCols+` FROM charges WHERE account_id=$1 ORDER BY effective_at, id`, req.AccountId)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	resp := &billingv1.ListChargesResponse{}
	for rows.Next() {
		c, err := scanCharge(rows)
		if err != nil {
			return nil, internal(err)
		}
		resp.Charges = append(resp.Charges, c.proto())
	}
	return resp, internal0(rows.Err())
}

// GetSummary reports the current cycle, the last statement and the derived delinquency.
func (s *Service) GetSummary(ctx context.Context, req *billingv1.GetSummaryRequest) (*billingv1.BillingSummary, error) {
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	acc, err := s.account(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	sum := &billingv1.BillingSummary{AccountId: acc.Id, AccountTime: acc.AccountTime, Stage: acc.Delinquency, Currency: acc.Currency}
	if acc.ActivatedAt == nil {
		return sum, nil
	}
	sts, err := s.loadStatements(ctx, acc.Id)
	if err != nil {
		return nil, internal(err)
	}
	credits, err := s.credits(ctx, acc.Id)
	if err != nil {
		return nil, err
	}
	act, now := acc.ActivatedAt.AsTime(), acc.AccountTime.AsTime()
	facts := make([]engine.StatementFacts, len(sts))
	for i, st := range sts {
		facts[i] = st.facts()
	}
	sum.CycleStart = timestamppb.New(engine.CycleClose(act, len(sts)))
	sum.CycleEnd = timestamppb.New(engine.CycleClose(act, len(sts)+1))
	if len(sts) > 0 {
		last := sts[len(sts)-1]
		sum.LastStatement, sum.MinimumDueMinor, sum.DueDate = last.proto(), last.Minimum, tsp(last.DueDate)
	}
	sum.Grace = engine.Grace(facts, credits)
	if acc.Delinquency != "charged_off" {
		d := engine.CapToOwed(engine.Delinquency(facts, credits, now), acc.OutstandingMinor)
		sum.PastDueMinor, sum.Dpd = d.PastDue, int32(d.DPD)
	}
	return sum, nil
}

func (s *Service) account(ctx context.Context, id string) (*accountsv1.Account, error) {
	r, err := s.accounts.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: id})
	if err != nil {
		return nil, err
	}
	return r.Account, nil
}

// credits are the payments (by received time) and waivers that pay toward minimums.
func (s *Service) credits(ctx context.Context, accountID string) ([]engine.Credit, error) {
	var out []engine.Credit
	req := &accountsv1.ListPaymentsRequest{AccountId: accountID, PageSize: 100}
	for {
		r, err := s.accounts.ListPayments(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, p := range r.Payments {
			c := engine.Credit{ReceivedAt: p.ReceivedAt.AsTime(), Amount: p.Amount.AmountMinor}
			if p.SettleEffectiveAt != nil && p.Status != accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLING {
				c.PostedAt = p.SettleEffectiveAt.AsTime()
			}
			switch p.Status {
			case accountsv1.PaymentStatus_PAYMENT_STATUS_PENDING, accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLING:
				c.Pending = true
			case accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED, accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNING:
			default:
				continue // failed, expired, returned: never paid
			}
			out = append(out, c)
		}
		if r.NextPageToken == "" {
			break
		}
		req.PageToken = r.NextPageToken
	}
	rows, err := s.pool.Query(ctx, `SELECT effective_at, amount_minor FROM waivers WHERE account_id=$1 AND status IN ('pending','posted')`, accountID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var c engine.Credit
		if err := rows.Scan(&c.ReceivedAt, &c.Amount); err != nil {
			return nil, internal(err)
		}
		c.PostedAt = c.ReceivedAt // waivers post at their own business date
		out = append(out, c)
	}
	return out, internal0(rows.Err())
}

// entries is the account's whole journal as billing sees it.
func (s *Service) entries(ctx context.Context, accountID string) ([]engine.Entry, error) {
	var out []engine.Entry
	req := &ledgerv1.ListEntriesRequest{AccountId: accountID}
	for {
		r, err := s.ledger.ListEntries(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, e := range r.Entries {
			out = append(out, engine.Entry{ID: e.Id, Reference: e.Reference, Type: e.EntryType, EffectiveAt: e.EffectiveAt.AsTime(), Delta: e.ReceivableDeltaMinor})
		}
		if r.NextPageToken == "" {
			return out, nil
		}
		req.PageToken = r.NextPageToken
	}
}

func event(typ, aggType, aggID string, after any) events.Event {
	ev := events.New(typ, aggType, aggID)
	ev.Actor = "billing"
	ev.After, _ = json.Marshal(after)
	return ev
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

func internal0(err error) error {
	if err == nil {
		return nil
	}
	return internal(err)
}
