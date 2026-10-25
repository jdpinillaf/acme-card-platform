-- Sprint 4: charge-off saga state (reserve → ledger → finalize, resumed by ResolvePending).
ALTER TABLE accounts
    ADD COLUMN charge_off_pending_at   timestamptz,
    ADD COLUMN charge_off_effective_at timestamptz,
    ADD COLUMN charge_off_amount       bigint CHECK (charge_off_amount >= 0);
ALTER TABLE accounts DROP CONSTRAINT accounts_restricted_reason_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_restricted_reason_check CHECK (restricted_reason IN ('payment_returned','over_limit'));
