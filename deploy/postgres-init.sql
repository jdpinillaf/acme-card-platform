-- Schema per service inside a single Postgres (local and RDS). Services also create their schema on
-- startup (pkg/db.Migrate), so this script is a convenience for local tooling only.
CREATE SCHEMA IF NOT EXISTS gateway;
CREATE SCHEMA IF NOT EXISTS accounts;
CREATE SCHEMA IF NOT EXISTS ledger;
CREATE SCHEMA IF NOT EXISTS audit;
