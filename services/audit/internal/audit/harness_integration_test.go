//go:build integration

package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/testutil"
	accmig "github.com/acme/acme-card/services/accounts/migrations"
	"github.com/acme/acme-card/services/audit/internal/store"
	auditmig "github.com/acme/acme-card/services/audit/migrations"
	billmig "github.com/acme/acme-card/services/billing/migrations"
	ledmig "github.com/acme/acme-card/services/ledger/migrations"
)

type fixture struct {
	svc     *Service
	pool    *pgxpool.Pool
	objects *MemStore
	store   *store.Store
}

func testKey() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("test audit signing key"))
	return ed25519.NewKeyFromSeed(seed[:])
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "accounts", accmig.FS, "."); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(dsn, "ledger", ledmig.FS, "."); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(dsn, "billing", billmig.FS, "."); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(dsn, "audit", auditmig.FS, "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.ConnectN(context.Background(), dsn, "audit", 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{pool: pool, objects: NewMemStore(), store: store.New(pool)}
	f.svc = NewService(pool, f.objects, testKey(), Config{ProducerSchemas: []string{"accounts"}, GapGrace: time.Nanosecond, StuckAfter: time.Minute})
	return f
}

// record appends n events for source, numbered from start.
func (f *fixture) record(t *testing.T, source string, seqs ...int64) {
	t.Helper()
	for _, s := range seqs {
		ev := events.New("x.happened", "account", fmt.Sprintf("%s-%d", source, s))
		ev.Source, ev.SourceSeq = source, s
		ev.After = []byte(fmt.Sprintf(`{"n":%d}`, s))
		if _, err := f.store.Record(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
}

// tamper runs sql with triggers disabled (test superuser only).
func (f *fixture) tamper(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role = origin`); err != nil {
		t.Fatal(err)
	}
}

// rehash recomputes every stored hash after a tamper, as an attacker with database access would.
func (f *fixture) rehash(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	rows, err := f.pool.Query(ctx, `SELECT chain_seq FROM audit_events ORDER BY chain_seq`)
	if err != nil {
		return err
	}
	var all []int64
	for rows.Next() {
		var s int64
		_ = rows.Scan(&s)
		all = append(all, s)
	}
	rows.Close()
	prev := store.GenesisHash
	for _, s := range all {
		ev, err := f.store.EventAt(ctx, s)
		if err != nil {
			return err
		}
		canon, _ := store.Canonical(ev, s)
		h := store.Hash(prev, canon)
		f.tamper(t, `UPDATE audit_events SET prev_hash=$2, hash=$3 WHERE chain_seq=$1`, s, prev, h)
		prev = h
	}
	f.tamper(t, `UPDATE chain_head SET hash=$1`, prev)
	return nil
}
