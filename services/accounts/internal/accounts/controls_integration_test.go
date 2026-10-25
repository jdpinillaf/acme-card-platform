//go:build integration

package accounts

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

func (f *fixture) events(t *testing.T, typ string) int {
	return f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload->>'type'=$1`, typ)
}

func TestSetFrozenTogglesAndEmits(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-freeze", 1000)
	r, err := f.svc.SetFrozen(ctx, &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: true})
	if err != nil || !r.Account.Frozen {
		t.Fatalf("freeze: %v %+v", err, r)
	}
	if _, err := f.svc.SetFrozen(ctx, &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: true}); err != nil {
		t.Fatal(err)
	}
	if n := f.events(t, "card.frozen"); n != 1 {
		t.Fatalf("card.frozen events=%d want 1 (re-freeze is a no-op)", n)
	}
	r, err = f.svc.SetFrozen(ctx, &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: false})
	if err != nil || r.Account.Frozen || f.events(t, "card.unfrozen") != 1 {
		t.Fatalf("unfreeze: %v %+v", err, r)
	}
	if r.Account.AvailableCreditMinor != 1000 {
		t.Fatalf("available=%d", r.Account.AvailableCreditMinor)
	}
}

func TestSetPurchaseLimitBounds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-limit", 1000)
	set := func(n int64) (*accountsv1.GetAccountResponse, error) {
		return f.svc.SetPurchaseLimit(ctx, &accountsv1.SetPurchaseLimitRequest{AccountId: acc.Id, PurchaseLimitMinor: n})
	}
	if r, err := set(500); err != nil || r.Account.PurchaseLimitMinor != 500 {
		t.Fatalf("500: %v %+v", err, r)
	}
	if r, err := set(0); err != nil || r.Account.PurchaseLimitMinor != 0 {
		t.Fatalf("0: %v %+v", err, r)
	}
	if _, err := set(1001); status.Code(err) != codes.InvalidArgument || grpcx.Reason(err) != "purchase_limit_above_credit_limit" {
		t.Fatalf("1001: %v", err)
	}
	if _, err := set(-1); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("-1: %v", err)
	}
	if n := f.events(t, "limits.updated"); n != 2 {
		t.Fatalf("limits.updated=%d want 2", n)
	}
	var before, after string
	_ = f.pool.QueryRow(ctx, `SELECT payload->'before'->>'purchase_limit_minor', payload->'after'->>'purchase_limit_minor' FROM outbox WHERE payload->>'type'='limits.updated' ORDER BY seq LIMIT 1`).Scan(&before, &after)
	if before != "0" || after != "500" {
		t.Fatalf("snapshot before=%s after=%s", before, after)
	}
}

func TestControlsOnClosedAccount(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-closed-ctl", 1000)
	if _, err := f.pool.Exec(ctx, `UPDATE accounts SET state='closed' WHERE id=$1`, acc.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetPurchaseLimit(ctx, &accountsv1.SetPurchaseLimitRequest{AccountId: acc.Id, PurchaseLimitMinor: 10}); grpcx.Reason(err) != "account_closed" {
		t.Fatalf("limit on closed: %v", err)
	}
	if _, err := f.svc.SetFrozen(ctx, &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: true}); err != nil {
		t.Fatalf("freeze on closed must be harmless: %v", err)
	}
}
