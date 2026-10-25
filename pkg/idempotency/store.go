package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrKeyReuseMismatch is returned when a key is reused with a different request fingerprint.
var ErrKeyReuseMismatch = errors.New("idempotency: key reused with a different request")

// ErrInProgress is returned when a duplicate waited longer than the wait timeout for the
// original request to finish. The caller should retry later with the same key.
var ErrInProgress = errors.New("idempotency: request with this key is still in progress")

// ErrClaimLost is returned by Complete when the claim's lease expired and another request took the
// key over; the result must not be stored (the new owner's will be).
var ErrClaimLost = errors.New("idempotency: claim lease expired and was taken over")

const (
	// finishTimeout bounds Complete/Abort, which run detached from the request context.
	finishTimeout = 5 * time.Second
	// defaultLease is how long a claim may stay in progress before a duplicate may take it over.
	// It must exceed the longest possible handler run (gateway request timeout + finish).
	defaultLease = 30 * time.Second
	// pollInterval is how often a duplicate re-reads the key while the original is in flight.
	pollInterval = 25 * time.Millisecond
)

// Result is what gets stored for replay once a request completes.
type Result struct {
	Status  int
	Body    []byte
	Outcome Outcome // defaults to OutcomeForStatus(Status) when empty
}

// Store persists idempotency keys in the gateway schema.
//
// A claim is a committed in_progress row with a lease and an owner: no transaction (and therefore no
// pool connection) stays open while the request runs. Duplicates poll the row until it completes,
// then replay. A row whose lease expired (its owner died) is taken over by the next request.
type Store struct {
	pool        *pgxpool.Pool
	ttl         time.Duration
	waitTimeout time.Duration
	lease       time.Duration
}

// NewStore builds a Store with the given key TTL.
func NewStore(pool *pgxpool.Pool, ttl time.Duration) *Store {
	return &Store{pool: pool, ttl: ttl, waitTimeout: 5 * time.Second, lease: defaultLease}
}

// WithWaitTimeout bounds how long a duplicate waits for the original request before answering
// in-progress. Waiting holds a goroutine, not a database connection.
func (s *Store) WithWaitTimeout(d time.Duration) *Store {
	s.waitTimeout = d
	return s
}

// WithLease sets how long a claim may stay in progress before another request may take it over.
func (s *Store) WithLease(d time.Duration) *Store {
	s.lease = d
	return s
}

// Claim is held by the request that must actually do the work. Exactly one of Complete or Abort
// must be called; until then duplicates wait (or, after the lease, take the key over).
type Claim struct {
	pool    *pgxpool.Pool
	keyHash string
	owner   uuid.UUID
	done    bool
}

// Acquire tries to take ownership of keyHash. Exactly one of (claim, replay) is non-nil on success.
func (s *Store) Acquire(ctx context.Context, keyHash, requestHash string) (*Claim, *Result, error) {
	owner := uuid.New()
	deadline := time.Now().Add(s.waitTimeout)
	for {
		// Insert first; or take over an in-progress row whose lease expired (same request only).
		// A concurrent inserter makes this statement wait until its insert commits, which is
		// immediate now that claims do not hold transactions open.
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO idempotency_keys (key_hash, request_hash, expires_at, lease_until, owner)
			VALUES ($1, $2, now() + $3::interval, now() + $4::interval, $5)
			ON CONFLICT (key_hash) DO UPDATE
			SET lease_until = EXCLUDED.lease_until, owner = EXCLUDED.owner
			WHERE idempotency_keys.status = 'in_progress'
			  AND coalesce(idempotency_keys.lease_until, '-infinity') < now()
			  AND idempotency_keys.request_hash = EXCLUDED.request_hash`,
			keyHash, requestHash, s.ttl, s.lease, owner)
		if err != nil {
			return nil, nil, fmt.Errorf("idempotency: claim: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return &Claim{pool: s.pool, keyHash: keyHash, owner: owner}, nil, nil
		}

		var (
			storedReq string
			status    string
			code      *int32
			body      []byte
			outcome   *string
		)
		err = s.pool.QueryRow(ctx, `
			SELECT request_hash, status, response_code, response_body, outcome
			FROM idempotency_keys WHERE key_hash = $1`, keyHash).
			Scan(&storedReq, &status, &code, &body, &outcome)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the owner aborted between our two statements: claim it now
		}
		if err != nil {
			return nil, nil, fmt.Errorf("idempotency: select: %w", err)
		}
		if storedReq != requestHash {
			return nil, nil, ErrKeyReuseMismatch
		}
		if status == "completed" {
			res := &Result{Status: int(*code), Body: body}
			if outcome != nil {
				res.Outcome = Outcome(*outcome)
			}
			return nil, res, nil
		}
		// In progress under a live lease: wait for the owner, without holding a connection.
		if time.Now().After(deadline) {
			return nil, nil, ErrInProgress
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
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
	tag, err := c.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'completed', response_code = $2, response_body = $3, outcome = $4, completed_at = now(), lease_until = NULL
		WHERE key_hash = $1 AND owner = $5 AND status = 'in_progress'`,
		c.keyHash, res.Status, res.Body, string(res.Outcome), c.owner)
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
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
	_, err := c.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE key_hash = $1 AND owner = $2 AND status = 'in_progress'`, c.keyHash, c.owner)
	if err != nil {
		return fmt.Errorf("idempotency: abort: %w", err)
	}
	return nil
}

// Sweep deletes keys whose TTL elapsed before now. Returns the number removed.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("idempotency: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}
