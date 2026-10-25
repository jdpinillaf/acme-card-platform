package accounts

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// MaxAdvanceDays bounds one simulated-clock advance (Sprint 4, dev only).
const MaxAdvanceDays = 400

// accountNow is the account's simulated time: the service clock plus the account's offset. It
// drives billing and the business date of every ledger posting (ADR-019).
func (s *Service) accountNow(acc accountRow) time.Time {
	return s.clock.Now().Add(time.Duration(acc.ClockOffset) * time.Second)
}

// acctNowSQL is accountNow in SQL, for reserve statements: the bound service time plus the offset
// of the account identified by accountExpr.
func acctNowSQL(nowArg, accountExpr string) string {
	return "(" + nowArg + "::timestamptz + make_interval(secs => (SELECT o.clock_offset_seconds FROM accounts o WHERE o.id = " + accountExpr + ")))"
}

// withEffective returns a copy of meta carrying the persisted business date for the ledger.
func withEffective(meta *commonv1.RequestMeta, at *time.Time) *commonv1.RequestMeta {
	var m *commonv1.RequestMeta
	if meta != nil {
		m = proto.Clone(meta).(*commonv1.RequestMeta)
	} else {
		m = &commonv1.RequestMeta{}
	}
	if at != nil {
		m.EffectiveAt = timestamppb.New(*at)
	}
	return m
}

// AdvanceClock moves the account's simulated clock forward by whole days. It only ever grows; the
// gateway exposes it only in the dev environment (DEV_CLOCK_ENABLED).
func (s *Service) AdvanceClock(ctx context.Context, req *accountsv1.AdvanceClockRequest) (*accountsv1.GetAccountResponse, error) {
	if req.Days < 1 || req.Days > MaxAdvanceDays {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_days", "days must be between 1 and 400")
	}
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		if before.State == StateClosed {
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		}
		acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET clock_offset_seconds = clock_offset_seconds + $2, version = version + 1, updated_at = now()
			WHERE id = $1 RETURNING `+accountCols, before.ID, int64(req.Days)*86400))
		if err != nil {
			return err
		}
		ev := event("account.clock_advanced", "account", acc.ID, req.Meta)
		ev.Before, ev.After = snapshot(map[string]any{"account_time": s.accountNow(before)}), snapshot(map[string]any{"account_time": s.accountNow(acc), "days": req.Days})
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return nil, err
	}
	return &accountsv1.GetAccountResponse{Account: s.accountProto(acc, v)}, nil
}
