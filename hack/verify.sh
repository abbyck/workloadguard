#!/usr/bin/env bash
# Prove that isolation blocks traffic between the sample workloads, and nothing else.
#
#   hack/verify.sh             full run against the deployed tool:
#                                1. baseline: everything reachable
#                                2. isolate gateway <-> dashboard: blocked; everything else,
#                                   including the dashboard's neighbour in tenant-b, reachable
#                                3. restart the gateway pods: the new ones are blocked too
#                                4. un-isolate: everything reachable again
#                                5. isolate all of tenant-a <-> all of tenant-b (allPods): the
#                                   neighbour is cut off too; traffic inside tenant-b still works
#                                6. un-isolate: everything reachable again
#   hack/verify.sh reachable   connectivity checks only, nothing isolated
#   hack/verify.sh blocked     ... with gateway <-> dashboard isolated
#   hack/verify.sh namespaces  ... with tenant-a <-> tenant-b isolated
#
# The full run reaches the tool through a port-forward to svc/workloadguard, or at WG_URL
# if set (e.g. WG_URL=http://localhost:8080 for a locally running binary).
#
# Each check execs into a source pod and makes a request with a short timeout. Prints a
# PASS/FAIL table and exits non-zero if any check doesn't match. If the run fails while
# isolation is on, isolation is turned off again before exiting.
set -euo pipefail

CTX=kind-workloadguard
TIMEOUT=3
# How long to give the CNI to program a policy change before checking.
SETTLE=3
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

A=(tenant-a deploy/gateway)
B=(tenant-b deploy/dashboard)
NEIGHBOUR=(tenant-b deploy/reports) # in B's namespace, but not B
BYSTANDER=(shared deploy/bystander)

failures=0
k() { kubectl --context "$CTX" "$@"; }

# probe NS TARGET CMD...: run CMD in TARGET, succeed if it exits 0.
probe() {
  local ns=$1 target=$2; shift 2
  k -n "$ns" exec "$target" -- "$@" >/dev/null 2>&1
}

http() { probe "$1" "$2" wget -q -O /dev/null -T "$TIMEOUT" "$3"; }
dns() { probe "$1" "$2" timeout "$TIMEOUT" nslookup "$3"; }

# check WANT DESC CMD...: run CMD, compare reachable/blocked against WANT, print a row.
# Retries a few times: Service endpoints and policy changes take a moment to be programmed
# after the API reports them. A real failure persists, so it still fails after the last try.
check() {
  local want=$1 desc=$2 got; shift 2
  for _ in 1 2 3; do
    if "$@"; then got=reachable; else got=blocked; fi
    [[ $got == "$want" ]] && break
    sleep 2
  done
  if [[ $got == "$want" ]]; then
    printf 'PASS  %-32s %s\n' "$desc" "$got"
  else
    printf 'FAIL  %-32s %s (want %s)\n' "$desc" "$got" "$want"
    failures=$((failures + 1))
  fi
}

# run_checks PEER_WANT NEIGHBOUR_WANT: expectations for A<->B and for A<->B's neighbour;
# every other path must be reachable, including traffic inside B's namespace.
run_checks() {
  local peer=$1 neighbour=$2
  check "$peer"      "A -> B"             http "${A[@]}" http://dashboard.tenant-b
  check "$peer"      "B -> A"             http "${B[@]}" http://gateway.tenant-a
  check "$neighbour" "A -> B's neighbour" http "${A[@]}" http://reports.tenant-b
  check "$neighbour" "B's neighbour -> A" http "${NEIGHBOUR[@]}" http://gateway.tenant-a
  check reachable    "B -> B's neighbour" http "${B[@]}" http://reports.tenant-b
  check reachable "A -> bystander"     http "${A[@]}" http://bystander.shared
  check reachable "B -> bystander"     http "${B[@]}" http://bystander.shared
  check reachable "bystander -> A"     http "${BYSTANDER[@]}" http://gateway.tenant-a
  check reachable "bystander -> B"     http "${BYSTANDER[@]}" http://dashboard.tenant-b
  check reachable "A -> DNS"           dns "${A[@]}" kubernetes.default.svc.cluster.local
  check reachable "B -> DNS"           dns "${B[@]}" kubernetes.default.svc.cluster.local
  # Trailing dot = fully qualified name, so the pod's search domains are skipped. Without it,
  # a search domain inherited from the host (e.g. a home router's) can return an empty answer
  # for example.com.<domain>, and Alpine's musl resolver stops there instead of trying further.
  check reachable "A -> external"      http "${A[@]}" http://example.com./
  check reachable "B -> external"      http "${B[@]}" http://example.com./
}

finish() {
  if ((failures > 0)); then
    echo "$failures check(s) failed"
    exit 1
  fi
  echo "all checks passed"
}

# Connectivity checks only.
if [[ $# -gt 0 ]]; then
  case $1 in
    reachable)  echo "== nothing isolated";                      run_checks reachable reachable ;;
    blocked)    echo "== gateway <-> dashboard isolated";        run_checks blocked reachable ;;
    namespaces) echo "== all of tenant-a <-> all of tenant-b";   run_checks blocked blocked ;;
    *) echo "usage: $0 [reachable|blocked|namespaces]" >&2; exit 2 ;;
  esac
  finish
fi

# Full run.
pf_pid=""
iso_id=""
cleanup() {
  if [[ -n $iso_id ]]; then
    echo "-- cleaning up: turning isolation $iso_id off" >&2
    # Ctrl-C also kills the port-forward, so fall back to deleting the isolation's policies
    # by label, which needs nothing but kubectl.
    curl -sf -m 10 -X DELETE "$WG_URL/v1/isolations/$iso_id" >/dev/null 2>&1 ||
      k delete networkpolicy -A -l "workloadguard.io/isolation-id=$iso_id" >&2 || true
  fi
  if [[ -n $pf_pid ]]; then kill "$pf_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT
# Turn Ctrl-C and kill into a normal exit, so the EXIT trap (and cleanup) still runs.
trap 'exit 130' INT TERM

if [[ -z ${WG_URL:-} ]]; then
  port=18090
  # Run kubectl directly, not through k(), so $! is kubectl's own PID and cleanup can kill it.
  kubectl --context "$CTX" -n workloadguard port-forward svc/workloadguard "$port:80" >/dev/null 2>&1 &
  pf_pid=$!
  WG_URL="http://localhost:$port"
  for _ in $(seq 20); do curl -sf "$WG_URL/readyz" >/dev/null 2>&1 && break; sleep 0.5; done
fi
if ! curl -sf "$WG_URL/readyz" >/dev/null; then
  echo "workloadguard is not reachable at $WG_URL (deploy it with: make deploy)" >&2
  exit 1
fi

# api METHOD PATH [BODY_FILE]: call the tool, print the body, fail on a non-2xx status.
api() {
  local args=(-sS -X "$1" -w '\n%{http_code}')
  [[ $# -ge 3 ]] && args+=(-H 'Content-Type: application/yaml' --data-binary "@$3")
  local out code
  out=$(curl "${args[@]}" "$WG_URL$2")
  code=${out##*$'\n'}
  out=${out%$'\n'*}
  echo "$out"
  [[ $code == 2* ]] || { echo "API $1 $2 returned $code" >&2; return 1; }
}

# isolate FILE: turn isolation on from a request file; remember the ID for cleanup.
isolate() {
  local resp
  resp=$(api POST /v1/isolations "$1")
  iso_id=$(grep -o '"id":"iso-[0-9a-f]*"' <<<"$resp" | cut -d'"' -f4)
  echo "isolation $iso_id: $(grep -o '"policies":\[[^]]*\]' <<<"$resp")"
  sleep "$SETTLE"
}

unisolate() {
  api DELETE "/v1/isolations/$iso_id" >/dev/null
  iso_id=""
  sleep "$SETTLE"
}

echo "== 1. baseline: everything reachable"
run_checks reachable reachable

echo "== 2. isolate tenant-a/gateway <-> tenant-b/dashboard"
isolate "$ROOT/examples/requests/isolate.yaml"
run_checks blocked reachable

echo "== 3. pod churn: restart the gateway pods, isolation must cover the new ones"
k -n tenant-a rollout restart deploy/gateway >/dev/null
k -n tenant-a rollout status deploy/gateway --timeout=120s >/dev/null
check blocked   "new A pods -> B"    http "${A[@]}" http://dashboard.tenant-b
check blocked   "B -> new A pods"    http "${B[@]}" http://gateway.tenant-a
check reachable "new A pods -> DNS"  dns "${A[@]}" kubernetes.default.svc.cluster.local

echo "== 4. un-isolate: previous connectivity restored"
unisolate
run_checks reachable reachable

echo "== 5. isolate all of tenant-a <-> all of tenant-b (allPods)"
isolate "$ROOT/examples/requests/isolate-namespaces.yaml"
run_checks blocked blocked

echo "== 6. un-isolate: previous connectivity restored"
unisolate
run_checks reachable reachable

finish
