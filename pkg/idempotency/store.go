package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrKeyReuseMismatch is returned when a key is reused with a different request fingerprint.
var ErrKeyReuseMismatch = errors.New("idempotency: key reused with a different request")

// ErrInProgress is returned when a duplicate waited longer than the wait timeout for the
// original request to finish. The caller should retry later with the same key.
var ErrInProgress = errors.New("idempotency: request with this key is still in progress")

const lockNotAvailable = "55P03"

// finishTimeout bounds Complete/Abort, which run detached from the request context.
const finishTimeout = 5 * time.Second

// Result is what gets stored for replay once a request completes.
type Result struct {
	Status  int
	Body    []byte
	Outcome Outcome // defaults to OutcomeForStatus(Status) when empty
}

// Store persists idempotency keys in the gateway schema.
type Store struct {
	pool        *pgxpool.Pool
	ttl         time.Duration
	waitTimeout time.Duration
}

// NewStore builds a Store with the given key TTL.
func NewStore(pool *pgxpool.Pool, ttl time.Duration) *Store {
	return &Store{pool: pool, ttl: ttl, waitTimeout: 5 * time.Second}
}

// WithWaitTimeout bounds how long a duplicate blocks (holding a pool connection) on the original.
func (s *Store) WithWaitTimeout(d time.Duration) *Store {
	s.waitTimeout = d
	return s
}

// Claim is held by the request that must actually do the work. Exactly one of Complete or Abort
// must be called; until then the key row stays locked and duplicates wait.
type Claim struct {
	tx      pgx.Tx
	keyHash string
	done    bool
}

// Acquire tries to take ownership of keyHash. Exactly one of (claim, replay) is non-nil on success.
func (s *Store) Acquire(ctx context.Context, keyHash, requestHash string) (*Claim, *Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("idempotency: begin: %w", err)
	}
	// Bound duplicate waiters so a slow downstream cannot pin the whole pool.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", s.waitTimeout.Milliseconds())); err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("idempotency: lock_timeout: %w", err)
	}
	// Insert first. If another transaction is inserting the same key, this statement blocks on the
	// primary key until that transaction commits or rolls back.
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (key_hash, request_hash, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (key_hash) DO NOTHING`, keyHash, requestHash, s.ttl)
	if err != nil {
		_ = tx.Rollback(ctx)
		if isLockTimeout(err) {
			return nil, nil, ErrInProgress
		}
		return nil, nil, fmt.Errorf("idempotency: insert: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return &Claim{tx: tx, keyHash: keyHash}, nil, nil
	}

	// Row exists. Lock it: waits if a crashed-but-recovering owner still holds it.
	var (
		storedReq string
		status    string
		code      *int32
		body      []byte
		outcome   *string
	)
	err = tx.QueryRow(ctx, `
		SELECT request_hash, status, response_code, response_body, outcome
		FROM idempotency_keys WHERE key_hash = $1 FOR UPDATE`, keyHash).
		Scan(&storedReq, &status, &code, &body, &outcome)
	if err != nil {
		_ = tx.Rollback(ctx)
		if isLockTimeout(err) {
			return nil, nil, ErrInProgress
		}
		return nil, nil, fmt.Errorf("idempotency: select: %w", err)
	}
	if storedReq != requestHash {
		_ = tx.Rollback(ctx)
		return nil, nil, ErrKeyReuseMismatch
	}
	if status == "in_progress" {
		// Orphaned row (owner died without completing). Take it over; we now hold the lock.
		return &Claim{tx: tx, keyHash: keyHash}, nil, nil
	}
	_ = tx.Rollback(ctx)
	res := &Result{Status: int(*code), Body: body}
	if outcome != nil {
		res.Outcome = Outcome(*outcome)
	}
	return nil, res, nil
}

// Complete stores the response and releases the key for replay.
func (c *Claim) Complete(ctx context.Context, res Result) error {
	if c.done {
		return errors.New("idempotency: claim already finished")
	}
	c.done = true
	// Detach from the request: a client disconnect or request deadline must not discard the result
	// of work that already happened downstream (that would let a retry run the effect twice).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if res.Outcome == "" {
		res.Outcome = OutcomeForStatus(res.Status)
	}
	_, err := c.tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_code = $2, response_body = $3, outcome = $4, completed_at = now()
		WHERE key_hash = $1`, c.keyHash, res.Status, res.Body, string(res.Outcome))
	if err != nil {
		_ = c.tx.Rollback(ctx)
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	if err := c.tx.Commit(ctx); err != nil {
		return fmt.Errorf("idempotency: commit: %w", err)
	}
	return nil
}

// Abort drops the claim without storing anything; the key becomes available again.
func (c *Claim) Abort(ctx context.Context) error {
	if c.done {
		return nil
	}
	c.done = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	return c.tx.Rollback(ctx)
}

// Sweep deletes keys whose TTL elapsed before now. Returns the number removed.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("idempotency: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// WithLease sets how long a claim may stay in progress before another request may take it over.
func (s *Store) WithLease(d time.Duration) *Store { return s }
