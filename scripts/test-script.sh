#!/usr/bin/env bash
# The environment is shared across failover stages so reservation identities and
# the original loyalty count survive separate Newman processes.
set -euo pipefail
cd "$(dirname "$0")/.."
variant=${1:-${VARIANT:-v2}}
service=${2:-${SERVICE_NAME:-loyalty}}
port=${3:-${PORT_NUMBER:-8050}}
newman=${NEWMAN_BIN:-newman}
[[ $variant == v2 && $service == loyalty && $port == 8050 ]] || {
  echo 'This implementation tests variant v2, service loyalty, port 8050.' >&2; exit 1;
}
work=$(mktemp -d /tmp/lab3-failover.XXXXXX)
stopped=false
cleanup() {
  result=$?
  trap - EXIT
  if $stopped; then docker compose start "$service" || true; fi
  rm -rf "$work"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cp "$variant/postman/environment.json" "$work/environment.json"
run_folder() {
  "$newman" run "$variant/postman/collection.json" --folder "$1" \
    --environment "$work/environment.json" --export-environment "$work/next.json" \
    --delay-request 100 --bail
  mv "$work/next.json" "$work/environment.json"
}
stop_service() {
  # Mark first so cleanup also handles an interrupted compose stop.
  stopped=true
  docker compose stop "$service"
}
start_service() {
  docker compose start "$service"
  ./scripts/wait-for.sh -t 120 "http://localhost:$port/manage/health"
  stopped=false
}
run_folder success
stop_service
run_folder step1
start_service
run_folder step2
stop_service
run_folder step3
start_service
run_folder step4
