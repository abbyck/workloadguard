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

expect() {
  local want=$1 desc=$2
  if reachable; then got=reachable; else got=blocked; fi
  if [[ $got == "$want" ]]; then
    echo "PASS  $desc ($got)"
  else
    echo "FAIL  $desc (want $want, got $got)"
    exit 1
  fi
}

kubectl --context "$CTX" create namespace "$NS" >/dev/null
k run server --image=nginxinc/nginx-unprivileged:1.27-alpine --labels=app=server --port=8080 >/dev/null
k expose pod server --port=8080 >/dev/null
k run client --image=curlimages/curl:8.11.1 --labels=app=client --command -- sleep infinity >/dev/null
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
sleep 3  # give the CNI a moment to program the rule
expect blocked "deny-all ingress applied"

k delete networkpolicy deny-all-ingress >/dev/null
sleep 3
expect reachable "policy removed"

echo "NetworkPolicy is enforced."
