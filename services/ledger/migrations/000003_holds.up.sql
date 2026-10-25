-- Credit reservations for card authorizations. Not postings: they only reduce available credit.
-- Status changes are evented through the outbox; money only moves at capture/refund (journals).
CREATE TABLE holds (
    id                uuid        PRIMARY KEY,
    account_id        uuid        NOT NULL,
    reference         text        NOT NULL UNIQUE,
    amount_minor      bigint      NOT NULL CHECK (amount_minor > 0),
    captured_minor    bigint      CHECK (captured_minor > 0),
    capture_reference text        UNIQUE,
    status            text        NOT NULL CHECK (status IN ('active','captured','released','expired')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz
);
CREATE INDEX holds_active_idx ON holds (account_id) WHERE status = 'active';
