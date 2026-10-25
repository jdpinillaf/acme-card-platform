//go:build integration

package ledger

import (
	"context"
	"testing"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

func pay(t *testing.T, s *Service, acc, ref string, amt int64) *ledgerv1.PostJournalResponse {
	t.Helper()
	r, err := s.PostPayment(context.Background(), &ledgerv1.PostPaymentRequest{AccountId: acc, Reference: ref, AmountMinor: amt})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func spend(t *testing.T, s *Service, acc, ref string, amt int64) {
	t.Helper()
	if r, err := hold(s, acc, ref, amt); err != nil || !r.Approved {
		t.Fatalf("hold %s: %v %+v", ref, err, r)
	}
	if _, err := capture(s, ref, "cap-"+ref, amt); err != nil {
		t.Fatal(err)
	}
}

func TestPostPaymentRestoresAvailable(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	spend(t, s, acc, "p1", 600)
	if a, _ := avail(t, s, acc); a != 400 {
		t.Fatalf("available=%d", a)
	}
	r1 := pay(t, s, acc, "payment:1", 600)
	r2 := pay(t, s, acc, "payment:1", 600)
	if !r2.Replayed || r2.JournalEntryId != r1.JournalEntryId {
		t.Fatalf("replay: %+v", r2)
	}
	if b := balances(t, s, acc); b[recv] != 0 {
		t.Fatalf("receivable=%d", b[recv])
	}
	if a, _ := avail(t, s, acc); a != 1000 {
		t.Fatalf("available=%d want 1000", a)
	}
	assertLedgerBalanced(t, s)
}

func TestPostPaymentReturnRaisesDebtBeyondLimit(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	spend(t, s, acc, "a", 1000)
	pay(t, s, acc, "payment:x", 1000)
	spend(t, s, acc, "b", 1000)
	if _, err := s.PostPaymentReturn(context.Background(), &ledgerv1.PostPaymentReturnRequest{AccountId: acc, Reference: "payment_return:x", AmountMinor: 1000}); err != nil {
		t.Fatal(err)
	}
	if b := balances(t, s, acc); b[recv] != 2000 {
		t.Fatalf("receivable=%d want 2000", b[recv])
	}
	if a, _ := avail(t, s, acc); a != 0 {
		t.Fatalf("available=%d want 0", a)
	}
	if r, _ := hold(s, acc, "c", 1); r.Approved {
		t.Fatal("no purchase while exposure exceeds the limit")
	}
	assertLedgerBalanced(t, s)
}

func TestPaymentEvents(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	spend(t, s, acc, "e", 300)
	pay(t, s, acc, "payment:e", 300)
	pay(t, s, acc, "payment:e", 300)
	_, _ = s.PostPaymentReturn(context.Background(), &ledgerv1.PostPaymentReturnRequest{AccountId: acc, Reference: "payment_return:e", AmountMinor: 300})
	_, _ = s.PostPaymentReturn(context.Background(), &ledgerv1.PostPaymentReturnRequest{AccountId: acc, Reference: "payment_return:e", AmountMinor: 300})
	for typ, want := range map[string]int{"ledger.payment_posted": 1, "ledger.payment_returned": 1} {
		var n int
		_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE payload->>'type'=$1`, typ).Scan(&n)
		if n != want {
			t.Errorf("%s=%d want %d", typ, n, want)
		}
	}
	var r int64
	_ = s.pool.QueryRow(context.Background(), `SELECT 0`).Scan(&r)
	if res, _ := s.GetBalances(context.Background(), &ledgerv1.GetBalancesRequest{AccountId: acc}); res.ReceivableMinor != 300 {
		t.Fatalf("GetBalances receivable_minor=%d want 300", res.ReceivableMinor)
	}
}
