//go:build integration

package ledger

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

func charge(s *Service, acc, ref string, kind ledgerv1.ChargeKind, amt int64) (*ledgerv1.PostJournalResponse, error) {
	return s.PostCharge(context.Background(), &ledgerv1.PostChargeRequest{AccountId: acc, Reference: ref, Kind: kind, AmountMinor: amt})
}

func rawBalance(t *testing.T, s *Service, acc, sub string) int64 {
	t.Helper()
	var raw int64
	if err := s.pool.QueryRow(context.Background(), `SELECT raw_balance_minor FROM balances WHERE account_id=$1 AND sub_account=$2`, acc, sub).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func entries(t *testing.T, s *Service, req *ledgerv1.ListEntriesRequest) []*ledgerv1.JournalEntry {
	t.Helper()
	var out []*ledgerv1.JournalEntry
	for {
		r, err := s.ListEntries(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r.Entries...)
		if r.NextPageToken == "" {
			return out
		}
		req.PageToken = r.NextPageToken
	}
}

func TestEffectiveAtDefaultsToNowAndHonoursMeta(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	when := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	if _, err := s.PostPayment(context.Background(), &ledgerv1.PostPaymentRequest{AccountId: acc, Reference: "payment:eff", AmountMinor: 100,
		Meta: &commonv1.RequestMeta{EffectiveAt: timestamppb.New(when)}}); err != nil {
		t.Fatal(err)
	}
	pay(t, s, acc, "payment:now", 100)
	got := map[string]time.Time{}
	for _, e := range entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc}) {
		got[e.Reference] = e.EffectiveAt.AsTime()
	}
	if !got["payment:eff"].Equal(when) {
		t.Fatalf("effective_at=%v want %v", got["payment:eff"], when)
	}
	var posted time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT posted_at FROM journal_entries WHERE reference='payment:now'`).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	if d := got["payment:now"].Sub(posted); d < -time.Second || d > time.Second {
		t.Fatalf("default effective_at %v vs posted_at %v", got["payment:now"], posted)
	}
}

func TestPostChargeKinds(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	for _, c := range []struct {
		ref  string
		kind ledgerv1.ChargeKind
		amt  int64
	}{
		{"interest:a:1", ledgerv1.ChargeKind_CHARGE_KIND_INTEREST, 1000},
		{"late_fee:a:1", ledgerv1.ChargeKind_CHARGE_KIND_FEE, 2500},
		{"waiver:i", ledgerv1.ChargeKind_CHARGE_KIND_INTEREST_WAIVER, 300},
		{"waiver:f", ledgerv1.ChargeKind_CHARGE_KIND_FEE_WAIVER, 2500},
	} {
		if _, err := charge(s, acc, c.ref, c.kind, c.amt); err != nil {
			t.Fatalf("%s: %v", c.ref, err)
		}
	}
	if r := rawBalance(t, s, acc, "receivable"); r != 700 {
		t.Fatalf("receivable=%d want 700", r)
	}
	if r := rawBalance(t, s, acc, "interest_income"); r != -700 {
		t.Fatalf("interest_income raw=%d want -700", r)
	}
	if r := rawBalance(t, s, acc, "fee_income"); r != 0 {
		t.Fatalf("fee_income raw=%d want 0", r)
	}
	types := map[string]string{}
	for _, e := range entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc}) {
		types[e.Reference] = e.EntryType
	}
	if types["interest:a:1"] != "interest_charged" || types["late_fee:a:1"] != "fee_charged" || types["waiver:i"] != "interest_waived" || types["waiver:f"] != "fee_waived" {
		t.Fatalf("entry types %v", types)
	}
	if _, err := charge(s, acc, "bad", ledgerv1.ChargeKind_CHARGE_KIND_UNSPECIFIED, 1); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified kind: %v", err)
	}
	assertLedgerBalanced(t, s)
}

func TestPostChargeIdempotentOnReference(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	replayed := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := charge(s, acc, "late_fee:x:1", ledgerv1.ChargeKind_CHARGE_KIND_FEE, 2500)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Replayed {
				mu.Lock()
				replayed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if replayed != 49 {
		t.Fatalf("replayed=%d want 49", replayed)
	}
	if r := rawBalance(t, s, acc, "receivable"); r != 2500 {
		t.Fatalf("receivable=%d", r)
	}
}

func TestPostChargeReferenceReuseDifferentAmount(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	if _, err := charge(s, acc, "late_fee:y:1", ledgerv1.ChargeKind_CHARGE_KIND_FEE, 2500); err != nil {
		t.Fatal(err)
	}
	if _, err := charge(s, acc, "late_fee:y:1", ledgerv1.ChargeKind_CHARGE_KIND_FEE, 2400); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("reuse: %v", err)
	}
}

func TestCollateralApplication(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	spend(t, s, acc, "c1", 30000)
	if _, err := s.PostCollateralApplication(context.Background(), &ledgerv1.PostCollateralApplicationRequest{AccountId: acc, Reference: "chargeoff:" + acc, AmountMinor: 30000}); err != nil {
		t.Fatal(err)
	}
	if r := rawBalance(t, s, acc, "collateral_held"); r != -70000 {
		t.Fatalf("collateral_held raw=%d", r)
	}
	if r := rawBalance(t, s, acc, "receivable"); r != 0 {
		t.Fatalf("receivable=%d", r)
	}
	acc2 := funded(t, s, 50000)
	if _, err := s.PostCollateralApplication(context.Background(), &ledgerv1.PostCollateralApplicationRequest{AccountId: acc2, Reference: "chargeoff:" + acc2, AmountMinor: 80000}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("over-collateral application: %v", err)
	}
	assertLedgerBalanced(t, s)
}

func TestSealPeriodClampsLaterPostings(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	T := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	seal := func(at time.Time) time.Time {
		r, err := s.SealPeriod(context.Background(), &ledgerv1.SealPeriodRequest{AccountId: acc, SealedThrough: timestamppb.New(at)})
		if err != nil {
			t.Fatal(err)
		}
		return r.SealedThrough.AsTime()
	}
	if got := seal(T); !got.Equal(T) {
		t.Fatalf("sealed=%v", got)
	}
	if _, err := s.PostPayment(context.Background(), &ledgerv1.PostPaymentRequest{AccountId: acc, Reference: "payment:late", AmountMinor: 100,
		Meta: &commonv1.RequestMeta{EffectiveAt: timestamppb.New(T.Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	if got := seal(T.Add(-2 * time.Hour)); !got.Equal(T) {
		t.Fatalf("seal moved back to %v", got)
	}
	for _, e := range entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc, EffectiveFrom: timestamppb.New(T)}) {
		if e.Reference == "payment:late" {
			if !e.EffectiveAt.AsTime().Equal(T) {
				t.Fatalf("clamped effective_at=%v want %v", e.EffectiveAt.AsTime(), T)
			}
			return
		}
	}
	t.Fatal("payment:late not found at or after the seal")
}

func TestListEntriesOrderAndRange(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	t1 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	t2, t3 := t1.Add(24*time.Hour), t1.Add(48*time.Hour)
	at := func(when time.Time) *commonv1.RequestMeta {
		return &commonv1.RequestMeta{EffectiveAt: timestamppb.New(when)}
	}
	if _, err := s.PostCharge(context.Background(), &ledgerv1.PostChargeRequest{AccountId: acc, Reference: "e3", Kind: ledgerv1.ChargeKind_CHARGE_KIND_FEE, AmountMinor: 3, Meta: at(t3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostCharge(context.Background(), &ledgerv1.PostChargeRequest{AccountId: acc, Reference: "e1", Kind: ledgerv1.ChargeKind_CHARGE_KIND_FEE, AmountMinor: 1, Meta: at(t1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostPayment(context.Background(), &ledgerv1.PostPaymentRequest{AccountId: acc, Reference: "e2", AmountMinor: 2, Meta: at(t2)}); err != nil {
		t.Fatal(err)
	}
	all := entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc, EffectiveFrom: timestamppb.New(t1), EffectiveTo: timestamppb.New(t3.Add(time.Second))})
	if len(all) != 3 || all[0].Reference != "e1" || all[1].Reference != "e2" || all[2].Reference != "e3" {
		t.Fatalf("order: %v", all)
	}
	if all[0].ReceivableDeltaMinor != 1 || all[1].ReceivableDeltaMinor != -2 {
		t.Fatalf("deltas: %d %d", all[0].ReceivableDeltaMinor, all[1].ReceivableDeltaMinor)
	}
	ranged := entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc, EffectiveFrom: timestamppb.New(t2), EffectiveTo: timestamppb.New(t3.Add(time.Second))})
	if len(ranged) != 2 || ranged[0].Reference != "e2" || ranged[1].Reference != "e3" {
		t.Fatalf("range: %v", ranged)
	}
	paged := entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc, EffectiveFrom: timestamppb.New(t1), EffectiveTo: timestamppb.New(t3.Add(time.Second)), PageSize: 1})
	if len(paged) != 3 || paged[2].Reference != "e3" {
		t.Fatalf("paged: %v", paged)
	}
	spend(t, s, acc, "cap", 50)
	for _, e := range entries(t, s, &ledgerv1.ListEntriesRequest{AccountId: acc}) {
		if e.EntryType == "purchase_captured" && e.ReceivableDeltaMinor != 50 {
			t.Fatalf("capture delta=%d", e.ReceivableDeltaMinor)
		}
	}
}
