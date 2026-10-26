#!/usr/bin/env bash
# Sprint 6 audit gate, run after the k6 load (ADR-021). Through the auditor API it snapshots every
# account, waits for the snapshots to reach the trail, anchors a checkpoint, verifies the chain
# (and, with DATABASE_URL set, a full walk via auditctl), replays 50 random accounts and runs the
# reconciliation. Any failure exits non-zero. Calls are paced under the per-subject rate limit.
#
#   set -a; . ./.env.dev; set +a; BASE_URL=http://localhost:8088 tests/load/audit_gate.sh
set -euo pipefail

BASE_URL=${BASE_URL:-http://localhost:8088}
SAMPLE=${SAMPLE:-50}
WAIT=${WAIT:-300}
cd "$(dirname "$0")/../.."
TOKEN=$(go run ./cmd/devtoken --sub auditor-gate --role auditor --ttl 1h)
fail=0

api() { # method path [body]; paced under the gateway's 60 requests/minute per subject
  sleep "${PACE:-1.05}"
  curl -sS -X "$1" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -H "Idempotency-Key: $(uuidgen)" ${3:+-d "$3"} -w '\n%{http_code}' "$BASE_URL$2"
}
check() { # name condition-result
  if [ "$2" = "true" ]; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi
}
body() { sed '$d' <<<"$1"; }
code() { tail -n1 <<<"$1"; }

# count events of a type recorded since $2 (RFC 3339), paging through the API
count_since() {
  local n=0 tok="" r
  while :; do
    r=$(api GET "/v1/audit/events?event_type=$1&from=$2&page_size=500${tok:+&page_token=$tok}")
    [ "$(code "$r")" = 200 ] || { echo "-1"; return; }
    n=$((n + $(body "$r" | jq '.events | length')))
    tok=$(body "$r" | jq -r '.next_page_token')
    [ -n "$tok" ] || break
  done
  echo "$n"
}

since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
r=$(api POST /v1/audit/snapshots '{}')
check "snapshot accepted (202)" "$([ "$(code "$r")" = 202 ] && echo true)"
want=$(body "$r" | jq '.count')
echo "      $want accounts queued"
deadline=$((SECONDS + WAIT))
while got=$(count_since snapshot.account "$since"); [ "$got" -lt "$want" ] && [ $SECONDS -lt $deadline ]; do sleep 10; done
check "all $want snapshots reached the trail (got $got)" "$([ "$got" -ge "$want" ] && echo true)"

r=$(api POST /v1/audit/checkpoints '{}')
check "checkpoint anchored" "$([ "$(code "$r")" = 201 ] && echo true)"

r=$(api GET /v1/audit/verify)
echo "      $(body "$r" | jq -c '{chain_valid, checked, incremental, checkpoints, checkpoints_valid, gaps: (.gaps|length), stuck: (.stuck_outbox|length)}')"
check "chain valid, checkpoints valid, 0 gaps, 0 stuck outboxes" \
  "$(body "$r" | jq '.chain_valid and .checkpoints_valid and (.gaps|length)==0 and (.stuck_outbox|length)==0')"

if [ -n "${DATABASE_URL:-}" ]; then
  if full=$(AWS_ENDPOINT_URL=${AWS_ENDPOINT_URL:-http://localhost:4566} go run ./services/audit/cmd/auditctl verify-chain); then
    check "full chain walk ($(jq '.checked' <<<"$full") events)" true
  else
    echo "$full"; check "full chain walk" false
  fi
fi

r=$(api GET "/v1/audit/events?event_type=account.opened&page_size=500")
ids=$(body "$r" | jq -r '.events[].aggregate_id' | sort -R | head -n "$SAMPLE")
diffs=0 replayed=0
for id in $ids; do
  r=$(api GET "/v1/audit/replay/$id")
  if [ "$(code "$r")" != 200 ]; then diffs=$((diffs + 1)); echo "      replay $id: HTTP $(code "$r")"; continue; fi
  d=$(body "$r" | jq '.differences | length')
  [ "$d" = 0 ] || { diffs=$((diffs + d)); echo "      replay $id: $(body "$r" | jq -c .differences)"; }
  replayed=$((replayed + 1))
done
check "replay of $replayed accounts: 0 differences (got $diffs)" "$([ "$diffs" = 0 ] && [ "$replayed" -gt 0 ] && echo true)"

r=$(api POST /v1/audit/reconciliation '{}')
echo "      $(body "$r" | jq -c '{total_violations, checks: (.checks|length)}')"
check "reconciliation: 0 violations" "$(body "$r" | jq '.total_violations == 0')"

exit $fail
