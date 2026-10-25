package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/outbox"
)

const chargeCols = `id::text, account_id::text, kind, cycle, reference, amount_minor, waived_minor, effective_at, status`

type chargeRow struct {
	ID, AccountID, Kind, Reference, Status string
	Cycle                                  int
	Amount, Waived                         int64
	EffectiveAt                            time.Time
}

func scanCharge(row pgx.Row) (chargeRow, error) {
	var c chargeRow
	err := row.Scan(&c.ID, &c.AccountID, &c.Kind, &c.Cycle, &c.Reference, &c.Amount, &c.Waived, &c.EffectiveAt, &c.Status)
	return c, err
}

func (c chargeRow) proto() *billingv1.Charge {
	return &billingv1.Charge{Id: c.ID, Kind: c.Kind, Cycle: int32(c.Cycle), AmountMinor: c.Amount, WaivedMinor: c.Waived,
		EffectiveAt: timestamppb.New(c.EffectiveAt), Status: c.Status}
}

var chargeKinds = map[string]ledgerv1.ChargeKind{"interest": ledgerv1.ChargeKind_CHARGE_KIND_INTEREST, "fee": ledgerv1.ChargeKind_CHARGE_KIND_FEE}

// terminal reports a ledger refusal that can never succeed on retry.
func terminal(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.InvalidArgument, codes.NotFound, codes.AlreadyExists:
		return true
	}
	return false
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

// postCharge is the charge saga: reserve a pending row on the unique reference → ledger PostCharge
// (detached) → mark posted and emit. A rerun with the same reference resumes or replays it.
func (s *Service) postCharge(ctx context.Context, accountID, kind string, cycle int, ref string, amount int64, effective time.Time) (bool, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO charges (id, account_id, kind, cycle, reference, amount_minor, effective_at, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'pending') ON CONFLICT (reference) DO NOTHING`,
		uuid.Must(uuid.NewV7()), accountID, kind, cycle, ref, amount, effective); err != nil {
		return false, internal(err)
	}
	c, err := scanCharge(s.pool.QueryRow(ctx, `SELECT `+chargeCols+` FROM charges WHERE reference=$1`, ref))
	if err != nil {
		return false, internal(err)
	}
	if c.Status != "pending" {
		return false, nil
	}
	return true, s.finishCharge(ctx, c)
}

func (s *Service) finishCharge(ctx context.Context, c chargeRow) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	_, err := s.ledger.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: c.AccountID, Reference: c.Reference, Kind: chargeKinds[c.Kind],
		AmountMinor: c.Amount, Meta: &commonv1.RequestMeta{Actor: "billing", EffectiveAt: timestamppb.New(c.EffectiveAt)}})
	next := "posted"
	if err != nil {
		if !terminal(err) {
			return err // stays pending: the next catch-up resumes it
		}
		slog.ErrorContext(ctx, "ledger refused a billing charge; marked failed", "reference", c.Reference, "err", err)
		next = "failed"
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE charges SET status=$2, posted_at=now() WHERE id=$1 AND status='pending'`, c.ID, next)
		if err != nil || tag.RowsAffected() == 0 || next != "posted" {
			return err
		}
		c.Status = next
		return outbox.Enqueue(ctx, tx, event(c.Kind+".charged", "charge", c.ID, c))
	})
}

// resumePending finishes charges left pending by a lost ledger response.
func (s *Service) resumePending(ctx context.Context, accountID string) error {
	rows, err := s.pool.Query(ctx, `SELECT `+chargeCols+` FROM charges WHERE account_id=$1 AND status='pending'`, accountID)
	if err != nil {
		return internal(err)
	}
	var pending []chargeRow
	for rows.Next() {
		c, err := scanCharge(rows)
		if err != nil {
			rows.Close()
			return internal(err)
		}
		pending = append(pending, c)
	}
	rows.Close()
	for _, c := range pending {
		if err := s.finishCharge(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
