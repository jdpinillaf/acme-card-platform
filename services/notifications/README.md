# notifications

Customer notifications (push, SMS, email; simulated providers) and HMAC-signed partner webhooks,
fed by the domain events on SNS → SQS (Sprint 7, ADR-022). gRPC on `:9006`. Design:
`docs/superpowers/specs/2026-10-25-sprint7-notifications-design.md`.
