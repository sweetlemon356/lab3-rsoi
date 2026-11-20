#!/usr/bin/env bash
# Run only on a disposable Compose stack: this test restarts Gateway, Loyalty and RabbitMQ.
set -euo pipefail
cd "$(dirname "$0")/.."
work=$(mktemp -d /tmp/lab3-broker-recovery.XXXXXX)
restore=false
cleanup() {
  result=$?
  trap - EXIT
  if $restore; then docker compose start rabbitmq loyalty gateway || true; fi
  rm -rf "$work"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
user="broker-recovery-$(date +%s)-$$"
request() {
  local method=$1 path=$2 expected=$3 data=${4:-}
  local status
  status=$(curl --silent --show-error --max-time 15 -o "$work/response.json" -w '%{http_code}' \
    -X "$method" -H "X-User-Name: $user" -H 'Content-Type: application/json' \
    --data "$data" "http://localhost:8080/api/v1$path")
  [[ $status == "$expected" ]] || { cat "$work/response.json"; echo "Expected $expected, got $status" >&2; exit 1; }
}

request POST /reservations 200 '{"hotelUid":"049161bb-badd-4fa8-9d90-87c9a82b0668","startDate":"2021-10-08","endDate":"2021-10-11"}'
uid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["reservationUid"])' "$work/response.json")
restore=true
docker compose stop loyalty
request DELETE "/reservations/$uid" 204
# Keep Gateway alive during broker restart to exercise the client's built-in recovery.
docker compose stop rabbitmq
docker compose up -d --no-deps --wait --wait-timeout 120 rabbitmq
./scripts/wait-for.sh -t 60 http://localhost:8080/manage/health
# Also verify that no process-local Gateway state is needed.
docker compose stop gateway
docker compose start loyalty gateway
./scripts/wait-for.sh -t 30 http://localhost:8050/manage/health
./scripts/wait-for.sh -t 30 http://localhost:8080/manage/health
for attempt in {1..60}; do
  request GET /loyalty 200
  if python3 -c 'import json,sys; sys.exit(json.load(open(sys.argv[1]))["reservationCount"] != 0)' "$work/response.json"; then break; fi
  sleep 0.5
done
python3 - "$work/response.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))["reservationCount"] == 0, "loyalty did not recover"
PY
request GET "/reservations/$uid" 200
python3 - "$work/response.json" <<'PY'
import json, sys
row = json.load(open(sys.argv[1]))
assert row["status"] == "CANCELED" and row["payment"]["status"] == "CANCELED", row
PY
# A second cancellation must not decrement loyalty again.
request DELETE "/reservations/$uid" 204
request GET /loyalty 200
python3 - "$work/response.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))["reservationCount"] == 0
PY
restore=false
echo 'RabbitMQ and Gateway restart recovery passed; compensation remains idempotent.'
