#!/usr/bin/env bash
# Check connectivity between the sample workloads in examples/.
#
# Each check execs into a source pod and makes a request with a short timeout.
# Peer paths (tenant-a/gateway <-> tenant-b/dashboard) are expected to be reachable now;
# once isolation exists (R5.1) they will be checked as blocked while it's on.
# Everything else (bystander, DNS, external) must always stay reachable.
#
# Usage: hack/verify.sh [reachable|blocked]   expected result for A<->B (default reachable)
#
# Prints a PASS/FAIL table and exits non-zero if any check doesn't match.
set -euo pipefail

PEER_WANT=${1:-reachable}
if [[ $PEER_WANT != reachable && $PEER_WANT != blocked ]]; then
  echo "usage: $0 [reachable|blocked]" >&2
  exit 2
fi

CTX=kind-workloadguard
TIMEOUT=3

A=(tenant-a deploy/gateway)
B=(tenant-b deploy/dashboard)
BYSTANDER=(shared deploy/bystander)

failures=0

# probe NS TARGET CMD...: run CMD in TARGET, succeed if it exits 0.
probe() {
  local ns=$1 target=$2; shift 2
  kubectl --context "$CTX" -n "$ns" exec "$target" -- "$@" >/dev/null 2>&1
}

http() { probe "$1" "$2" wget -q -O /dev/null -T "$TIMEOUT" "$3"; }
dns() { probe "$1" "$2" timeout "$TIMEOUT" nslookup "$3"; }

# check WANT DESC CMD...: run CMD, compare reachable/blocked against WANT, print a row.
check() {
  local want=$1 desc=$2 got; shift 2
  if "$@"; then got=reachable; else got=blocked; fi
  if [[ $got == "$want" ]]; then
    printf 'PASS  %-32s %s\n' "$desc" "$got"
  else
    printf 'FAIL  %-32s %s (want %s)\n' "$desc" "$got" "$want"
    failures=$((failures + 1))
  fi
}

# run_checks PEER_WANT: PEER_WANT is the expectation for A<->B; every other path must be reachable.
run_checks() {
  local peer=$1
  check "$peer"   "A -> B"             http "${A[@]}" http://dashboard.tenant-b
  check "$peer"   "B -> A"             http "${B[@]}" http://gateway.tenant-a
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

echo "== A<->B expected $PEER_WANT"
run_checks "$PEER_WANT"

if ((failures > 0)); then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
