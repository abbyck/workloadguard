# workloadguard

A small Go service that runs inside a Kubernetes cluster and does two things on request:

- **Isolate:** blocks all traffic between two workloads (each picked by namespace and label
  selector) using NetworkPolicies. Their other traffic keeps working, and turning isolation
  off restores the connectivity they had before.
- **Harden:** finds workloads in one or more namespaces that have no resource requests/limits
  or no hardened `securityContext`, and patches them. A dry run shows the changes first.

Everything it creates is labeled `app.kubernetes.io/managed-by=workloadguard`, so you can
inspect it with `kubectl` without the tool. It is built with plain client-go and tested on kind.

## API

Operators drive workloadguard through a small HTTP API. Request bodies can be JSON or YAML
(`Content-Type: application/yaml`); responses are always JSON. The service is only exposed
inside the cluster, so reach it with a port-forward:

```sh
kubectl -n workloadguard port-forward svc/workloadguard 8080:80
```

Example request bodies are in [examples/requests/](examples/requests/).

| Method and path | What it does |
|---|---|
| `POST /v1/isolations` | Turn isolation on between two workloads. Returns the isolation ID. |
| `GET /v1/isolations` | List active isolations. |
| `GET /v1/isolations/{id}` | Show one isolation and the objects it owns. |
| `DELETE /v1/isolations/{id}` | Turn isolation off. |
| `POST /v1/hardening/plan` | Dry run: show what hardening would change. Changes nothing. |
| `POST /v1/hardening/apply` | Apply the same plan and report the result per workload. |
| `GET /healthz`, `GET /readyz` | Liveness, and readiness (the Kubernetes API is reachable). |

### Isolation

```sh
# on
curl -sS -X POST localhost:8080/v1/isolations \
  -H 'Content-Type: application/yaml' --data-binary @examples/requests/isolate.yaml

# list
curl -sS localhost:8080/v1/isolations

# off
curl -sS -X DELETE localhost:8080/v1/isolations/<id>
```

Request ([examples/requests/isolate.yaml](examples/requests/isolate.yaml)):

```yaml
isolate:
  a: {namespace: tenant-a, selector: {app: gateway}}
  b: {namespace: tenant-b, selector: {app: dashboard}}
```

Response:

```json
{
  "id": "iso-3f9a2c1b",
  "a": {"namespace": "tenant-a", "selector": {"app": "gateway"}},
  "b": {"namespace": "tenant-b", "selector": {"app": "dashboard"}},
  "createdAt": "2026-10-05T12:00:00Z",
  "policies": [
    {"namespace": "tenant-a", "name": "workloadguard-iso-3f9a2c1b"},
    {"namespace": "tenant-b", "name": "workloadguard-iso-3f9a2c1b"}
  ],
  "warnings": []
}
```

- **The ID is derived from the request**, as a hash of both sides. Sending the same request again
  returns `200` with the existing isolation instead of creating a second one (`201` the first time).
  Swapping `a` and `b` gives the same ID.
- **Selectors are exact label matches** (`matchLabels`). `matchExpressions` aren't supported,
  because the tool has to build "everything except these pods", and negating arbitrary
  expressions gets error-prone fast.
- **`DELETE` is safe to repeat.** It removes only the objects labeled with that ID and returns
  what it deleted. If they're already gone it returns `200` with an empty list.
- **Existing NetworkPolicies on the targets block the request** with `409` and a list of those
  policies. Adding isolation on top of them would widen access, because NetworkPolicies are
  combined with OR. See [Decisions](#decisions) (to be written).
- **`warnings`** flags things that are allowed but suspicious, such as a selector that currently
  matches no pods.

### Hardening

```sh
# dry run
curl -sS -X POST localhost:8080/v1/hardening/plan \
  -H 'Content-Type: application/yaml' --data-binary @examples/requests/harden-baseline.yaml

# apply
curl -sS -X POST localhost:8080/v1/hardening/apply \
  -H 'Content-Type: application/yaml' --data-binary @examples/requests/harden-baseline.yaml
```

Request ([examples/requests/harden-baseline.yaml](examples/requests/harden-baseline.yaml)):

```yaml
namespaces: [legacy]
level: baseline   # or strict
```

Plan response:

```json
{
  "level": "baseline",
  "workloads": [
    {
      "kind": "Deployment", "namespace": "legacy", "name": "unhardened",
      "restartsPods": true,
      "changes": [
        {"container": "", "field": "securityContext.seccompProfile.type", "before": null, "after": "RuntimeDefault"},
        {"container": "web", "field": "securityContext.allowPrivilegeEscalation", "before": null, "after": false},
        {"container": "web", "field": "resources.requests.memory", "before": null, "after": "64Mi"}
      ]
    }
  ],
  "skipped": [
    {"kind": "Pod", "namespace": "legacy", "name": "debug", "reason": "bare Pod: reported, not patched"}
  ]
}
```

`container: ""` means a pod-level field. The apply response is the same plan with a `result` on
each workload: `patched`, `unchanged` or `failed` (with an `error`), plus its rollout status
(`ready`, or not ready within the timeout). Apply works out the plan again when it runs, so it
reports what it actually changed, even if the cluster changed since the dry run.

### Errors

Errors return a JSON body: `{"error": {"code": "namespace_not_found", "message": "..."}}`.

| Status | When |
|---|---|
| `400` | Invalid request: missing fields, empty selector, `a` same as `b`, unknown level, missing namespace |
| `403` | The request targets a protected namespace or workload (system namespaces, the tool's own, or labeled `workloadguard.io/ignore=true`) |
| `404` | Unknown isolation ID on `GET` |
| `409` | Isolation would combine with existing NetworkPolicies on the targets |
| `500` / `503` | The Kubernetes API failed or is unreachable. The body says what, if anything, was already changed. |

### Why an HTTP API and not a CRD

A CRD with a controller is the production-grade design: requests are declarative and stored in
the cluster, access is controlled with normal RBAC, they work with GitOps, and status is written
back to the object. It also costs a lot more: CRD schema, code generation, a reconcile loop and
its edge cases. For this assignment, an imperative API built on plain client-go fits the time
budget and keeps the logic easy to follow. All state still lives in the cluster as labeled
objects, so moving to a CRD later would mostly mean replacing the HTTP layer.

The API has no authentication. It's reachable only from inside the cluster or through
`kubectl port-forward`, which already requires cluster access. In production it would need
authentication and authorization, or the CRD approach above.
