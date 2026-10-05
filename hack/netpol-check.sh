#!/usr/bin/env bash
# Prove the cluster's CNI enforces NetworkPolicy:
#   1. client can reach server
#   2. a deny-all ingress policy on the server blocks it
#   3. deleting the policy restores access
# Runs in a throwaway namespace that is deleted on exit.
set -euo pipefail

CTX=kind-workloadguard
NS=wg-netpol-check
k() { kubectl --context "$CTX" -n "$NS" "$@"; }

cleanup() { kubectl --context "$CTX" delete namespace "$NS" --ignore-not-found --wait=false >/dev/null; }
trap cleanup EXIT

reachable() {
  k exec client -- curl -s -o /dev/null --max-time 3 http://server:8080/ 2>/dev/null
}

# expect WANT DESC: retry for a while, because Service endpoints and policy changes take a
# moment to be programmed after the API reports them. A real failure persists, so it still
# fails after the last attempt.
expect() {
  local want=$1 desc=$2 got
  for _ in 1 2 3 4 5; do
    if reachable; then got=reachable; else got=blocked; fi
    [[ $got == "$want" ]] && break
    sleep 2
  done
  if [[ $got == "$want" ]]; then
    echo "PASS  $desc ($got)"
  else
    echo "FAIL  $desc (want $want, got $got)"
    exit 1
  fi
}

# A previous run's namespace may still be terminating; creating it again would fail.
kubectl --context "$CTX" wait --for=delete "namespace/$NS" --timeout=90s >/dev/null 2>&1 || true
kubectl --context "$CTX" create namespace "$NS" >/dev/null
# Zero grace period: these throwaway pods (sleep ignores SIGTERM) would otherwise keep the
# namespace terminating for 30s after the script ends.
fast='{"spec":{"terminationGracePeriodSeconds":0}}'
k run server --image=nginxinc/nginx-unprivileged:1.27-alpine --labels=app=server --port=8080 --overrides="$fast" >/dev/null
k expose pod server --port=8080 >/dev/null
k run client --image=curlimages/curl:8.11.1 --labels=app=client --overrides="$fast" --command -- sleep infinity >/dev/null
k wait --for=condition=Ready pod/server pod/client --timeout=120s >/dev/null

expect reachable "baseline: client -> server"

k apply -f - >/dev/null <<'YAML'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-all-ingress
spec:
  podSelector:
    matchLabels:
      app: server
  policyTypes: [Ingress]
YAML
expect blocked "deny-all ingress applied"

k delete networkpolicy deny-all-ingress >/dev/null
expect reachable "policy removed"

echo "NetworkPolicy is enforced."
