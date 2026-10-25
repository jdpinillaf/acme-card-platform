-- Sprint 6: the audit service publishes its own events (reconciliation results, alerts) through
-- the same outbox → SNS → SQS path, so they are chained like everything else.
CREATE TABLE outbox (
    seq          bigserial   PRIMARY KEY,
    event_id     uuid        NOT NULL UNIQUE,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (seq) WHERE published_at IS NULL;
CREATE TABLE outbox_sequence (
    source   text   PRIMARY KEY,
    next_seq bigint NOT NULL CHECK (next_seq >= 1)
);

CREATE TABLE audit_reconciliations (
    run_id           uuid        PRIMARY KEY,
    ran_at           timestamptz NOT NULL,
    total_violations bigint      NOT NULL,
    report           jsonb       NOT NULL,
    s3_key           text        NOT NULL
);

-- Replay looks up an account's ledger postings by the account id inside the event.
CREATE INDEX audit_events_journal_account_idx ON audit_events ((after->>'account_id'), chain_seq) WHERE event_type = 'ledger.journal_posted';
CREATE INDEX audit_events_account_state_idx ON audit_events (aggregate_id, chain_seq) WHERE event_type IN ('account.changed','snapshot.account');
