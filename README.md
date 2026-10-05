# workloadguard

A small Go service that runs inside a Kubernetes cluster and does two things on request:

- **Isolate:** blocks all traffic between two workloads (each picked by namespace and label
  selector) using NetworkPolicies. Their other traffic keeps working, and turning isolation
  off restores the connectivity they had before.
- **Harden:** finds workloads in one or more namespaces that have no resource requests/limits
  or no hardened `securityContext`, and patches them. A dry run shows the changes first.

Everything it creates is labeled `app.kubernetes.io/managed-by=workloadguard`, so you can
inspect it with `kubectl` without the tool. It is built with plain client-go and tested on kind.

## Quick start

Prerequisites, with the versions this was built and tested with:

- Go 1.27 (only for `make test` / `make vet`; the image builds in Docker)
- Docker (tested with 29)
- kind v0.33. The cluster config pins kind v0.33's node image (Kubernetes 1.37), and kind
  only guarantees a node image with the kind release it was built for. Any kind from v0.24
  enforces NetworkPolicy with its default CNI.
- kubectl (tested with 1.37)
- Internet access, to pull the kind node image and the sample images

```sh
make all            # = make cluster samples deploy verify
```

That runs four steps:

1. `make cluster` creates a kind cluster named `workloadguard` (one control-plane node, two
   workers, pinned node image). `hack/netpol-check.sh` proves its CNI enforces NetworkPolicy.
2. `make samples` deploys the sample workloads in [examples/](examples/):
   - `tenant-a/gateway` and `tenant-b/dashboard` to isolate
   - `shared/bystander`, which must keep working
   - `legacy/unhardened` and `legacy/partial` to harden
3. `make deploy` builds the image, loads it into kind and deploys [deploy/](deploy/).
4. `make verify` runs [hack/verify.sh](hack/verify.sh): baseline, isolate, check blocking,
   restart pods, un-isolate, check restored.

Every target uses the `kind-workloadguard` context explicitly, never your current one.
`make help` lists the targets, and `make clean` deletes the cluster.

To try it by hand:

```sh
make port-forward   # in another terminal: API on localhost:8080

# isolate, look, un-isolate
curl -sS -X POST localhost:8080/v1/isolations \
  -H 'Content-Type: application/yaml' --data-binary @examples/requests/isolate.yaml
curl -sS localhost:8080/v1/isolations
hack/verify.sh blocked
curl -sS -X DELETE localhost:8080/v1/isolations/<id>

# see what hardening would change, then apply it
curl -sS -X POST localhost:8080/v1/hardening/plan \
  -H 'Content-Type: application/yaml' --data-binary @examples/requests/harden-baseline.yaml
curl -sS -X POST localhost:8080/v1/hardening/apply \
  -H 'Content-Type: application/yaml' --data-binary @examples/requests/harden-baseline.yaml
```

To run the binary outside the cluster instead (it uses your kubeconfig):

```sh
go run ./cmd/workloadguard --context kind-workloadguard
```

Run `go run ./cmd/workloadguard -h` for all flags: protected namespaces, pod CIDRs,
resource defaults, and rollout and shutdown timeouts.

### Installing with Helm (optional)

The plain manifests in [deploy/](deploy/) are the primary path. [charts/workloadguard](charts/workloadguard)
renders the same objects with default values; I checked this by comparing the two renders
object by object. The one exception is the Namespace: Helm keeps its release record in the
release namespace, so a chart can't create it. Create and label it first, so Pod Security
Admission `restricted` is still enforced (`--create-namespace` can't add the label):

```sh
kubectl create namespace workloadguard
kubectl label namespace workloadguard pod-security.kubernetes.io/enforce=restricted
helm install workloadguard charts/workloadguard -n workloadguard
```

Values cover the image, protected namespaces, pod CIDRs, hardening defaults and rollout
timeout, shutdown timeout, log level, resources, the NetworkPolicy (with an `ingressFrom`
list, say for a Prometheus namespace), pod annotations, node selector, tolerations and
affinity. `values.schema.json` rejects unknown keys and malformed values at install time. Replicas aren't a value; the tool runs
one on purpose. `hack/verify.sh` passes against a Helm install too.

### Repository layout

```
cmd/workloadguard/    main: flags, client, HTTP server, graceful shutdown
internal/api/         HTTP handlers, request decoding, error mapping
internal/isolation/   validation, NetworkPolicy building, on/off/list, conflict check
internal/hardening/   pod-spec rules, plan/apply, rollout status
internal/guard/       never-touch rules shared by both features
internal/kube/        client setup (in-cluster or kubeconfig)
deploy/               the tool's own manifests (namespace, RBAC, deployment, service)
examples/             sample workloads and example requests
hack/                 kind config, cluster setup, NetworkPolicy check, verify.sh
charts/workloadguard/ optional Helm chart (same objects as deploy/)
```

### Libraries

- **[client-go](https://github.com/kubernetes/client-go)**, as the brief recommends, with
  `k8s.io/api` and `k8s.io/apimachinery`. The typed clientset and its fake cover both
  features. From apimachinery I used:
  - label selectors, to evaluate policies in tests and find pods
  - name and label validation
  - `strategicpatch`, to compute minimal patches
  - `wait`, for rollout polling

  I didn't use controller-runtime or Kubebuilder. The tool is request-driven with no
  reconcile loop, so a controller framework would add structure without solving anything
  here.
- **[sigs.k8s.io/yaml](https://github.com/kubernetes-sigs/yaml)**, to accept YAML or JSON
  request bodies with one strict parser that rejects unknown fields.
- **The standard library** for everything else: `net/http` with Go 1.22+ method routing,
  `log/slog` for JSON logs, and `net/netip` for CIDRs.
- **[staticcheck](https://staticcheck.dev/)** in CI, via `go run`, so it isn't a dependency.

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
| `POST /v1/hardening/undo/plan`, `/undo/apply` | Show, then revert, what hardening set (see [Undo](#undo)). |
| `GET /healthz`, `GET /readyz` | Liveness, and readiness (the Kubernetes API is reachable). |
| `GET /metrics` | Prometheus metrics (see [Metrics](#metrics)). |

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
  "id": "iso-13893ed2ba",
  "a": {"namespace": "tenant-a", "selector": {"app": "gateway"}},
  "b": {"namespace": "tenant-b", "selector": {"app": "dashboard"}},
  "createdAt": "2026-10-05T12:09:38Z",
  "policies": [
    {"namespace": "tenant-a", "name": "workloadguard-iso-13893ed2ba-a"},
    {"namespace": "tenant-b", "name": "workloadguard-iso-13893ed2ba-b"}
  ],
  "warnings": []
}
```

- **The ID is derived from the request**, as a hash of both sides. Sending the same request again
  returns `200` with the existing isolation instead of creating a second one (`201` the first time).
  Swapping `a` and `b` gives the same ID, so the response lists the sides in a fixed order,
  which may not be the order you sent.
- **Selectors are exact label matches** (`matchLabels`). `matchExpressions` aren't supported,
  because the tool has to build "everything except these pods", and negating arbitrary
  expressions gets error-prone fast.
- **`DELETE` is safe to repeat.** It removes only the objects labeled with that ID and returns
  what it deleted. If they're already gone it returns `200` with an empty list.
- **Existing NetworkPolicies on the targets block the request** with `409` and a list of those
  policies. Adding isolation on top of them would widen access, because NetworkPolicies are
  combined with OR. See [Existing NetworkPolicies](#existing-networkpolicies-are-refused-not-merged).
- **Host-network pods are refused** with `422`. NetworkPolicy doesn't apply to them, so
  isolating one would report success and block nothing.
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

Plan response (trimmed; this is the real output for the sample `legacy` namespace):

```json
{
  "level": "baseline",
  "dryRun": true,
  "workloads": [
    {
      "kind": "Deployment", "namespace": "legacy", "name": "unhardened",
      "restartsPods": true,
      "changes": [
        {"container": "", "field": "securityContext.seccompProfile.type", "before": null, "after": "RuntimeDefault"},
        {"container": "web", "field": "securityContext.allowPrivilegeEscalation", "before": null, "after": false},
        {"container": "web", "field": "resources.requests.cpu", "before": null, "after": "50m"},
        {"container": "web", "field": "resources.requests.memory", "before": null, "after": "64Mi"},
        {"container": "web", "field": "resources.limits.memory", "before": null, "after": "256Mi"}
      ],
      "notes": [
        "container web gets a 256Mi memory limit; if it uses more it will be OOM-killed, possibly long after the rollout. Check its usage first (kubectl top pod --containers)"
      ]
    }
  ],
  "skipped": [],
  "unchanged": [],
  "notes": []
}
```

- `container: ""` is a pod-level field. `before` is always `null`, because the tool only fills
  unset fields.
- `skipped` lists workloads it won't patch, with the reason: bare Pods, Jobs, CronJobs,
  workloads managed by an operator, or anything labeled `workloadguard.io/ignore=true`.
- `unchanged` lists workloads that already meet the level.
- `notes` on a workload say what was deliberately left alone, or what the level might break.

The apply response is the same structure with `dryRun: false`, and each workload gets:

- `result`: `patched` or `failed`, with an `error`. A failure doesn't stop the other workloads.
- `rollout`: `ready`, or what went wrong, for example
  `rolled out, but pods are failing: pod unhardened-54bf… container web: Error (exit 1)`.

Apply works the plan out again from the live objects. It reports what it actually changed,
even if something changed since the dry run.

### Metrics

`GET /metrics` serves Prometheus metrics from a dedicated registry:

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `workloadguard_http_requests_total` | counter | `route`, `method`, `code` | `route` is the pattern, e.g. `/v1/isolations/{id}`, so IDs don't create new series; unknown paths are `unmatched` |
| `workloadguard_http_request_duration_seconds` | histogram | `route` | Buckets up to 3 minutes, because hardening apply waits for rollouts |
| `workloadguard_isolations_active` | gauge | | Counted from the cluster at scrape time (one list call). It stays right across restarts and manual deletes, and is left out of a scrape if the count fails, rather than reported as 0 |
| `workloadguard_hardening_workloads_total` | counter | `operation` (`hardening`/`undo`), `result` (`patched`/`failed`) | |
| `workloadguard_kube_api_requests_total` | counter | `method`, `code` | Every Kubernetes API call, through client-go's metrics hooks. A rise in `403` means missing RBAC |
| `workloadguard_kube_api_request_duration_seconds` | histogram | `verb` | |
| `workloadguard_build_info` | gauge | `version`, `goversion` | Always 1; tells you which build is running (also `--version` and the startup log) |

Plus the standard Go runtime and process metrics.

### Errors

Errors return a JSON body: `{"error": {"code": "namespace_not_found", "message": "..."}}`.

| Status | When |
|---|---|
| `400` | Invalid request: missing fields, empty selector, `a` same as `b`, unknown level, missing namespace |
| `403` | The request targets a protected namespace or workload (system namespaces, the tool's own, or labeled `workloadguard.io/ignore=true`) |
| `404` | Unknown isolation ID on `GET` |
| `409` | Isolation would combine with existing NetworkPolicies on the targets |
| `422` | The targets can't be isolated with NetworkPolicy (a matching pod uses `hostNetwork`) |
| `500` | A Kubernetes API call failed. The body says what, if anything, was already changed. |
| `503` | Only from `/readyz`: the Kubernetes API is unreachable. |

### Why an HTTP API and not a CRD

A CRD with a controller is the production-grade design: requests are declarative and stored in
the cluster, access is controlled with normal RBAC, they work with GitOps, and status is written
back to the object. It also costs a lot more: CRD schema, code generation, a reconcile loop and
its edge cases. For this assignment, an imperative API built on plain client-go fits the time
budget and keeps the logic easy to follow. All state still lives in the cluster as labeled
objects, so moving to a CRD later would mostly mean replacing the HTTP layer.

The API has no authentication, so a NetworkPolicy in the tool's namespace blocks all
ingress: no pod can call it. Before I added it, any pod in the cluster could reach the
Service and get the tool to patch workloads it had no rights to; I checked that on kind.
`kubectl port-forward` still works, because it connects inside the pod's network namespace,
and it already requires cluster access. In production the API would need authentication
and authorization, or the CRD approach above.

## Decisions

The brief leaves these open:

| Decision | Where |
|---|---|
| How isolation behaves as pods come and go | [Pods coming and going](#pods-coming-and-going) |
| What counts as a hardened securityContext | [Two levels](#what-hardened-means-two-levels) |
| Set default requests/limits or only report them, and which defaults | [Resource defaults](#resource-defaults-set-them-conservatively) |
| Which workload types the tool handles | [Which workloads are handled](#which-workloads-are-handled) |
| How operators trigger actions and see what the tool did or would do | [API](#api), [Why an HTTP API](#why-an-http-api-and-not-a-crd), [Seeing what would change](#seeing-what-would-change) |
| Which namespaces or workloads the tool never touches | [Never-touch list](#never-touch-list) |

My own decisions beyond those:

- [How the policies express "everything except B"](#how-isolation-works)
- [Refusing to combine with existing NetworkPolicies](#existing-networkpolicies-are-refused-not-merged)
- [Keeping state in the cluster](#state-and-retries)
- [Running the tool](#running-the-tool): one replica, RBAC

### Isolation

#### How isolation works

NetworkPolicy can only allow traffic. There is no "deny A to B" rule. So isolation creates
one policy per side. Each selects that side's pods for both ingress and egress, which makes
them deny-by-default, and then allows everything except the other side:

1. Pods in every namespace other than the peer's.
2. Pods in the peer's namespace that don't match the peer's selector. The selector is an AND
   of labels, so "not the peer" is an OR, `k1≠v1 OR k2≠v2`. That takes one rule per label,
   because conditions inside a single rule are ANDed. Combining them into one rule would cut
   off pods that share only one label with the peer.
3. An `ipBlock` for everything that isn't a pod: external addresses, nodes (kubelet probes)
   and host-network pods. **It excludes the pod CIDRs.** kindnet, like most CNIs, matches
   `ipBlock` against pod IPs as well. With a bare `0.0.0.0/0`, the peer gets straight back in.
   I checked this on kind: removing the exclusion reopened A↔B.

Either policy alone blocks A↔B. Having both means one deleted or broken policy doesn't
reopen the connection.

The cluster's pod CIDR isn't available through the Kubernetes API, so it's the
`--pod-cidrs` flag (default `10.244.0.0/16`, kind's default). Every isolation request
checks that each node's `spec.podCIDRs` falls inside it, and refuses with `500` if not.
A node added later with a new range is caught instead of silently leaking.

#### Pods coming and going

Policies select pods by label, not by name or IP. New replicas, restarts, rescheduled
pods and scale-ups are covered as soon as they start. Nothing has to watch for them.
I checked this on kind by restarting the gateway (all pods replaced, new IPs) and
scaling the dashboard up while isolated; the new pods were blocked straight away.

What follows from this:

- **A pod whose labels change leaves or joins the isolation** with them. That's the
  NetworkPolicy model, and it's consistent with how Services select pods.
- **Connections already open when isolation turns on may survive.** Enforcement depends
  on the CNI, and some only check new connections. To contain a live compromise, restart
  the pods after isolating them.
- **A selector matching no pods is allowed**, with a warning in the response. The policy
  applies to pods created later, but it's often a typo.

#### Existing NetworkPolicies are refused, not merged

If any NetworkPolicy already selects either side's pods, the request fails with `409` and
lists those policies. This includes a namespace-wide default deny (empty `podSelector`).

NetworkPolicies combine with OR. Say the dashboard only accepts traffic from one client
([examples/extras/preexisting-netpol.yaml](examples/extras/preexisting-netpol.yaml)).
Adding "allow everything except the gateway" would open it to every namespace: the
opposite of what an operator isolating a compromise wants. It would also make
"restore previous connectivity" meaningless, because the tool would have changed what
the old policy allowed.

**Other workloadguard isolations count as conflicts too.** "Everything except B" plus
"everything except C" on the same pods adds up to "everything", so two isolations on the
same workload would cancel each other out. The tool refuses the second instead.

The trade-off: you can't isolate a workload that already has NetworkPolicies, or isolate
one workload from two peers at once. The production answer is a policy API with real deny
rules that take priority over NetworkPolicies instead of being combined with them:
[ClusterNetworkPolicy](https://network-policy-api.sigs.k8s.io/) (v1alpha2, which replaced
AdminNetworkPolicy in 2026 and is heading for beta), or CNI-specific policies (Calico,
Cilium).

#### Turning isolation off

Isolation only ever adds policies, and it refuses to combine with existing ones. So
deleting what it added restores the previous connectivity exactly. Off deletes only the
policies labeled with that isolation's ID, with a UID check, so other policies in the
same namespaces, including other isolations, are untouched. Deleting something already
gone is a success, so retries are safe.

#### State and retries

The tool keeps no state in memory. Isolations are the labeled NetworkPolicies
themselves, and the original request is stored as an annotation. The ID is a hash of
the request with the sides in a fixed order:

- Re-sending a request, or swapping `a` and `b`, finds the same isolation instead of
  making a second one.
- Restarting the tool changes nothing. Isolation keeps blocking while it's down, because
  the CNI enforces the policies, not the tool.
- Policies are written with server-side apply, so re-sending a request also repairs
  manual edits to them.
- If the second policy fails, the first is rolled back (unless it already existed), so a
  failed request doesn't leave half an isolation behind.
- Cluster changes run under a context that ignores client disconnects, with their own
  timeout. A dropped `kubectl port-forward` can't stop a request halfway.

#### Which requests are rejected

- **Empty selectors.** They would select every pod in the namespace.
- **Selectors that could match the same pod, in the same namespace.** `{app: gateway}` and
  `{tier: edge}` are rejected even if no pod carries both labels yet, because one could
  later, and a pod can't be isolated from itself. They count as separate only when some
  label key has different values on each side.
- **Protected targets**, with `403`. These are system namespaces, the tool's own namespace,
  and anything labeled `workloadguard.io/ignore=true`, including the pods the selector
  currently matches.
- **Unknown fields**, such as `selecter:`. A typo shouldn't silently turn into an empty
  selector.

Selectors are exact label matches only (`matchLabels`), as in the brief's example.
Supporting `matchExpressions` would mean negating arbitrary expressions, which is where
bugs come from.

### Hardening

#### Which workloads are handled

| Kind | What happens | Why |
|---|---|---|
| Deployment, StatefulSet, DaemonSet | **Patched** (pod template) | Their controllers roll the change out to new pods. |
| Deployment, StatefulSet, DaemonSet owned by another controller (an operator's custom resource) | Skipped, naming the owner | The operator would put its own template back, so a patch would only cause repeated rollouts. Harden it through the owning resource. |
| ReplicaSet owned by a Deployment, and its pods | Ignored | Covered by patching the Deployment. |
| ReplicaSet with no owner, or another owner (e.g. Argo Rollouts) | **Reported**, not patched | Nothing here would patch it correctly; fix its manifest or owner. |
| Pod owned by a StatefulSet, DaemonSet or Job | Ignored | Covered by its controller's row. |
| Pod owned by anything else (an operator creating pods directly) | **Reported**, not patched | Set the fields through the owner. |
| Static pod (owned by a Node) | Ignored | It comes from a file on the node; the API can't change it. |
| Bare Pod (no owner) | **Reported**, not patched | Most securityContext fields can't change on a running pod, and nothing would recreate it. |
| Job | **Reported**, not patched | A Job's pod template can't be changed. |
| CronJob | **Reported**, not patched | It could be patched, but it's out of scope here; fix its manifest. |
| Anything labeled `workloadguard.io/ignore=true` | Skipped, with the reason | Owner opted out. |
| Workloads in protected namespaces | Request refused (`403`) | System namespaces and the tool's own. |

Init containers are hardened along with regular containers. Ephemeral (debug) containers
aren't.

#### What "hardened" means: two levels

One rule decides which level a field belongs to. **Baseline** only holds fields that don't
change what a normal process can do at startup. **Strict** holds fields that need knowledge
of the image to apply safely.

| Field | Baseline | Strict |
|---|---|---|
| pod `seccompProfile: RuntimeDefault` | ✓ | ✓ |
| `allowPrivilegeEscalation: false` | ✓ | ✓ |
| `capabilities.drop: [ALL]` (explicit `add` entries are kept) | | ✓ |
| pod `runAsNonRoot: true` | | ✓ |
| `readOnlyRootFilesystem: true` | | ✓ |

**Why dropping all capabilities isn't in baseline.** I tested it on kind. Images that start
as root and then switch user crash without capabilities:

- `nginx:1.27-alpine` fails with
  `chown(... /var/cache/nginx/client_temp, 101) failed (Operation not permitted)`.
- `redis:7.4-alpine` fails with `setpriv: setresuid failed`.

Both run again with `CHOWN, SETUID, SETGID` (nginx) or `SETUID, SETGID` (redis) added back.
The tool can't see an image's user through the Kubernetes API. Server-side dry run doesn't
catch it either, because the patch is valid and the failure only shows when the container
starts. So it's opt-in.

The trade-off is that baseline does not meet Pod Security Standards **restricted**. Strict
gets close.

What strict does to the samples, also tested on kind:

- `legacy/unhardened` (nginx-unprivileged) crash-loops on the read-only root filesystem.
- `legacy/partial`'s busybox init container is refused by the kubelet with
  `CreateContainerConfigError`, because it runs as root and `runAsNonRoot` is set.

The apply response reports both.

**The tool never loosens or overrides an explicit value.** It only fills unset fields:

- An explicit `allowPrivilegeEscalation: true` stays.
- `runAsNonRoot` isn't set if the pod or any container explicitly has `runAsUser: 0`. That
  combination makes the kubelet refuse to start the container. The tool adds a note instead.
- **Privileged containers** keep their securityContext unchanged and get a note. They're
  usually privileged for a reason (CNI agents, storage drivers), and the API rejects
  `allowPrivilegeEscalation: false` together with `privileged: true`.
- Containers that add `SYS_ADMIN` keep `allowPrivilegeEscalation` unset, for the same API
  reason.

#### Resource defaults: set them, conservatively

The tool **sets** missing values instead of only reporting them. A container with no requests
is scheduled as if it costs nothing, and one with no memory limit can take memory from
everything else on its node. Both are configurable by flag.

| Field | Default | Note |
|---|---|---|
| `requests.cpu` | `50m` | Not set if the container has a CPU limit |
| `requests.memory` | `64Mi` | Not set if the container has a memory limit |
| `limits.memory` | `256Mi` | Raised to the request if the request is higher; the API rejects a limit below it |
| `limits.cpu` | **never set** | |

- **No CPU limit.** CPU limits throttle a container even when the node has CPU to spare,
  which causes latency for no gain. A memory limit protects the node; a CPU limit mostly
  hurts the workload.
- **A limit without a request is left alone.** The API server sets the request equal to the
  limit at admission. Filling in the small default would lower it and change the pod's QoS
  class from Guaranteed to Burstable.
- **Namespaces with a LimitRange that sets container defaults** keep resources untouched.
  The LimitRange already applies them at admission, and it's the namespace owner's call.
  The plan says so in `notes`.
- **A new memory limit is the riskiest change.** A container that really uses more than
  256Mi will be OOM-killed, possibly hours after the rollout, long after the settle check.
  So the plan adds a note to every container that gets one, telling the operator to check
  its usage first (`kubectl top pod --containers`). Setting the limit from observed usage
  is on the more-time list.

#### Seeing what would change

`POST /v1/hardening/plan` returns every change per workload and container, with the field,
old value and new value, and whether it restarts pods. It changes nothing.

Each patch is also sent as a **server-side dry run**, so API validation, admission webhooks
and Pod Security Admission get their say before anything changes. Plan and apply share the
same code: apply is the plan without the dry-run flag.

#### Applying without breaking things

- **Strategic merge patch** carrying only the added fields. Containers are merged by name,
  so a concurrent change to an image or another field isn't overwritten.
- The patch includes the object's **`resourceVersion`**. If the workload changed since it
  was read, the API server rejects the patch with a conflict (checked on kind). The tool
  then reads it again, recomputes and retries. A stale value is never written.
- **A failure on one workload doesn't stop the others.** Each workload gets its own result.
- **Re-applying is a no-op.** Nothing is patched and no pods restart.
- **Rollout status is reported.** Changing the pod template restarts the pods (the plan
  says `restartsPods: true`). Apply waits up to `--rollout-timeout` (default 90s) for each
  rollout, then:
  - watches the pods for another 10 seconds, and
  - reports any container that is crash-looping, refused by the kubelet, or has restarted.

The 10-second settle check comes from running strict on kind. `unhardened` has no readiness
probe, so Kubernetes counted the new pod as available the moment its container started. The
rollout "completed", and the old pod was removed before nginx crashed a second later.
Without the settle check, the tool reported that as `ready`.

The tool reports a failed rollout. It doesn't roll it back. For Deployments, the old pods
keep serving whenever the new ones never become available, as happened with `partial`.

#### Undo

Every patched workload records what was changed, in annotations:

- `workloadguard.io/hardening-changes`: a JSON list of `{container, field, before, after}`.
  Later runs add to it, for example baseline and then strict.
- `workloadguard.io/hardened-at` and `workloadguard.io/hardening-level`.

`POST /v1/hardening/undo/plan` and `/undo/apply` take the same body as hardening, minus
the level. For each recorded change:

- The field is removed **only if it still holds the value the tool set**. If someone has
  changed it since, it's theirs now: it's left alone and reported in `notes`. Undo must not
  revert someone else's later edit.
- Quantities compare by value, so `500m` equals `0.5`.
- A container that no longer exists is reported, not an error.

Structs and maps the revert leaves empty are dropped. So the spec returns to exactly what it
was, not to one with an extra `securityContext: {}`. Then the annotations are removed. If
every field has drifted, only the annotations are removed, which restarts nothing.

Undo goes through the same path as apply: a strategic merge patch with the
`resourceVersion`, conflict retries, per-workload results and rollout status.

I checked it on kind: hardening `legacy` and then undoing it left both pod templates
identical to the originals.

**GitOps and drift.** I checked on kind what re-applying the original manifest does to the
fields hardening added. With `kubectl apply` (client-side, Argo CD's default) and with
server-side apply (how Flux applies), they **stay**: both only manage the fields in the
manifest. With `kubectl replace`, or a sync that deletes and recreates, they're lost. So
GitOps usually doesn't undo hardening, but the cluster silently drifts from Git: the
hardened values exist only in the cluster. The real fix belongs in the source manifests.

### Never-touch list

Both features go through one shared guard (`internal/guard`), so their rules can't drift
apart.

- **Protected namespaces:** `kube-system`, `kube-public`, `kube-node-lease`,
  `local-path-storage` (kind's storage provisioner), and the tool's own namespace. That last
  one comes from `POD_NAMESPACE`, or the service account, and is always added.
  - Isolating or patching anything there could break the cluster itself (DNS, CNI, storage)
    or the tool.
  - Requests that target a protected namespace get `403` from both features.
  - `--protected-namespaces` *replaces* the default list, so clusters with other system
    namespaces (a CNI's or a monitoring stack's) can add them. Removing an entry is possible
    too, which is the operator's call. The startup log prints the final list.
- **Opt-out label:** `workloadguard.io/ignore=true` on a namespace or a workload.
  - Isolation refuses with `403` if the namespace, or any pod the selector currently
    matches, carries it.
  - Hardening skips labeled workloads and lists them under `skipped` with the reason.
  - Only the exact value `true` counts. A typo like `yes` is ignored, so it can't silently
    protect something.

### Running the tool

- **One replica.** Actions are request-driven and all state lives in the cluster as labeled
  objects, so there's nothing to share between replicas or recover after a restart. A
  replacement pod picks up exactly where the last one stopped. On SIGTERM the pod finishes
  in-flight requests (up to `--shutdown-timeout`, 20s) before exiting. I checked this by
  deleting the pod 3 seconds into a hardening apply: the request still returned a complete
  `200`. An apply that waits longer than the shutdown timeout for rollouts loses its report,
  but each workload's patch is atomic, so the cluster stays consistent; re-running the
  request reports them as unchanged.
- **The tool passes its own checks.** Its pod runs as non-root with a read-only root
  filesystem, all capabilities dropped and the RuntimeDefault seccomp profile, with
  resources and probes set. Its namespace enforces Pod Security Admission `restricted`, a
  hardening plan against it, at both levels, finds nothing to change, and a NetworkPolicy
  blocks all ingress (see [Why an HTTP API](#why-an-http-api-and-not-a-crd)).
- **Least-privilege RBAC, cluster-scoped.** Isolation and hardening target namespaces chosen
  per request, so the role can't be namespaced. Every rule maps to an API call the tool
  makes, with no wildcards:

  | Resource | Verbs | Used for |
  |---|---|---|
  | namespaces | get | Target exists; opt-out label |
  | pods | list | Selector matches (isolation); bare Pods and rollout failures (hardening) |
  | nodes | list | Checking `--pod-cidrs` covers every node |
  | limitranges | list | Leave resources to a LimitRange with defaults |
  | networkpolicies | get, list, create, patch, delete | Isolation on (server-side apply needs `create` for new objects), off, conflicts |
  | deployments, statefulsets, daemonsets | get, list, patch | Hardening and rollout status |
  | replicasets | list | Reported only, unless owned by a Deployment |
  | jobs, cronjobs | list | Reported only |

  Testing in the cluster caught one gap: server-side apply of a new NetworkPolicy also
  needs `create`, not just `patch`.

## Tests

```sh
make test     # unit tests, with the race detector
make vet      # gofmt, go vet, staticcheck (what CI runs)
make verify   # end-to-end isolation proof against the deployed tool
make integration   # Go tests against the kind cluster, then make verify
```

### Unit tests

The unit tests use the client-go fake clientset. Most of the logic is pure functions, so
many tests don't need a client at all:

- building policies, negating selectors, validation and the overlap check
- the pod-spec hardening rules
- rollout status and the guard rules

The fake clientset covers the parts that read and write the cluster: on/off, conflicts,
plan/apply, conflict retries and rollback. The HTTP layer is tested end to end against a
fake cluster, which pins down every status code the API section documents.

**What the unit tests don't prove.** The fake clientset doesn't enforce NetworkPolicies, run
admission, honor `DryRun`, or check `resourceVersion` on patches. So:

- Traffic blocking is proved by `hack/verify.sh` on a real cluster.
- The tests intercept dry-run patches themselves.
- The `resourceVersion` conflict was checked against kind by hand.

### The tricky cases, and why I chose them

Each of these has a comment in the test explaining it.

1. **Negating a multi-label selector** (`TestPeersAllowEverythingExceptB`, in
   `internal/isolation/policy_test.go`). "Everything except B" for B =
   `{app: dashboard, tier: frontend}` needs one rule per label, because conditions inside a
   single rule are ANDed. Combining them into one rule looks right and passes a casual
   review. But it cuts off pods that share just one label with B, like
   `{app: dashboard, tier: backend}`.

   The test evaluates the generated rules with real label-selector matching against nine
   pods, instead of comparing structs. I checked that it catches the bug by introducing the
   bug on purpose: three cases fail.
2. **The `ipBlock` must exclude pod CIDRs** (`TestIPBlockExceptsPodCIDRs`). The obvious
   "allow `0.0.0.0/0` for external traffic" silently lets the isolated peer back in, because
   kindnet matches `ipBlock` against pod IPs. This one was found and proved on kind:
   removing the exclusion reopened A↔B. The unit test keeps it from regressing.
3. **Turning off deletes only what it created** (`TestOffRemovesOnlyItsOwnPolicies`). The
   brief stresses restoring previous connectivity. A broader delete, such as every
   workloadguard policy in the namespace or every policy selecting the pods, would remove
   another team's policy or another isolation, and so change connectivity the request
   didn't own.
4. **Existing policies, including other isolations, block the request**
   (`TestOnRefusesExistingPolicies`). Because NetworkPolicies combine with OR, "add a policy"
   can *widen* access. Two isolations on the same pods cancel each other out. This is easy to
   miss, because each isolation looks correct on its own.
5. **Hardening keeps explicit values** (`TestExplicitValuesArePreserved`, and
   `TestApplyKeepsExplicitValuesThroughThePatch` for the real patch path). A
   partially-configured container keeps every value it has: requests, an explicit
   `allowPrivilegeEscalation: true`, `runAsUser`, added capabilities. Only the gaps are
   filled. The memory limit is raised to the request when the default would be lower, and
   `runAsNonRoot` is skipped when something explicitly runs as root. Getting this wrong
   breaks the workload, which the brief names as the thing to avoid.

### Integration tests

These are behind the `integration` build tag and run against the kind cluster, in throwaway
namespaces they delete afterwards. They cover what the fake clientset can't:

- **Isolation:** real server-side apply. The stored policy matches what was built and is
  owned by the `workloadguard` field manager. Re-applying is a no-op, a stacked isolation
  conflicts, and off removes everything.
- **Hardening:**
  - plan sends a real server-side dry run, which validates and stores nothing
    (`generation` unchanged)
  - apply rolls out Ready, and re-applying is a no-op
  - undo returns the template to exactly what the API server stored at creation, server
    defaults included
- **Concurrency:** a patch built before someone else's edit is refused by the API server
  with a `Conflict`, because it carries the old `resourceVersion`.

`make integration` runs them and then `hack/verify.sh`. CI does the same in its `e2e` job,
on a fresh kind cluster for every push.

### End-to-end: `hack/verify.sh`

It runs against the deployed tool, through its own port-forward:

1. Baseline: every path is reachable.
2. Isolate through the API. A↔B is blocked, while bystander↔A, bystander↔B, DNS and
   external egress still work.
3. Restart the gateway pods. The new pods are blocked too.
4. Un-isolate. Every path is reachable again.

It prints a PASS/FAIL table and exits non-zero on any mismatch. If it's interrupted while
isolated, it turns isolation off before exiting, falling back to `kubectl` if Ctrl-C also
killed the port-forward. `hack/verify.sh reachable|blocked` runs the connectivity checks
alone.

## Known limitations

### Isolation

- **NetworkPolicy is L3/L4, and the CNI enforces it.** "No traffic at all" holds only as far
  as the CNI implements the spec. Connections open before isolation may survive on CNIs that
  only check new connections, so restart the pods if that matters.
- **Host-network pods can't be isolated.** NetworkPolicy doesn't apply to them, and their
  traffic carries the node's IP, which the `ipBlock` allows. The tool refuses a request
  whose selector matches one (`422`), but a host-network pod that starts matching later
  isn't caught.
- **Traffic through a NodePort or LoadBalancer can get around it.** With
  `externalTrafficPolicy: Cluster`, kube-proxy may rewrite the source address to a node IP,
  which the `ipBlock` allows. The same goes for traffic relayed through a third pod. The
  tool blocks direct pod-to-pod traffic only.
- **Workloads with existing NetworkPolicies can't be isolated**, and neither can one workload
  from two peers at once. The tool refuses rather than silently widening access.
  ClusterNetworkPolicy or CNI deny policies would remove this limit.
- **Policies added after isolation aren't watched.** If someone later adds a NetworkPolicy
  that selects an isolated pod, its allow rules combine with isolation and can reopen
  traffic. The opt-out label is likewise only checked when the request is made.
- **`--pod-cidrs` must be correct.** Every request checks it against each node's
  `spec.podCIDRs`, but CNIs that do their own IP allocation don't set that field. The tool
  then can only warn.

### Hardening

- **It can't know an image.** A user, writable paths or capabilities an image needs can't
  be seen through the API. That's why baseline is below Pod Security Standards
  `restricted` and strict is opt-in.
- **It restarts pods and doesn't roll back.** A failed rollout is reported, not undone.
- **It creates drift from Git.** Argo CD and Flux usually leave the added fields alone, so
  the cluster no longer matches the manifests, and a delete-and-recreate sync loses the
  hardening. The real fix belongs in the source manifests (see [Undo](#undo)).
- **Apply restarts every selected workload at once.** Each rolls out with its own strategy,
  but they all start together. Hardening a busy namespace is best done in a quiet period.
- **The defaults are flat values**, not based on observed usage. Something like the Vertical
  Pod Autoscaler's recommendations would be better.
- **The settle check is a heuristic.** It watches pods for 10 seconds after a rollout, so a
  crash later than that is missed; readiness probes would catch it properly. The rollout
  report is also lost if apply runs longer than the pod's shutdown timeout. The patches
  themselves still land atomically.
- **LimitRange detection is coarse.** Any container default in the namespace means no
  resource field is set there.
- **Only Deployments, StatefulSets and DaemonSets are patched.**

### The service

- **No authentication or authorization on the API.** A NetworkPolicy stops other pods from
  reaching it, so the way in is `kubectl port-forward`, which already needs cluster access.
  But anyone who can port-forward to the tool can isolate or patch with its permissions.
- **The audit trail is the logs plus the labels and annotations on what the tool touched.**
  It doesn't emit Kubernetes Events.
- **One replica**, so requests fail while the pod is being replaced.
- **The end-to-end CI job builds a whole kind cluster** on every push, which takes a few
  minutes. A larger project would run it less often, for example on pull requests only.

### What I'd do with more time

1. **CRDs and a controller** instead of the HTTP API (`Isolation` and `HardeningPolicy`
   resources). Requests become declarative and RBAC-controlled. The controller can
   reconcile drift, such as deleted policies or new conflicting ones, and the history lives
   in the cluster.
2. **ClusterNetworkPolicy** (formerly AdminNetworkPolicy), or Cilium/Calico policies, for
   isolation. Real deny rules remove the conflict limitation and the `ipBlock`/pod-CIDR
   coupling.
3. **Admission-time enforcement** with Pod Security Admission, Kyverno or
   ValidatingAdmissionPolicy, so unhardened workloads are stopped or fixed when they're
   created instead of patched afterwards.
4. Emit Kubernetes Events for every action, and watch for host-network pods or new
   policies that start matching an active isolation.
5. Set memory limits from observed usage (metrics-server or VPA recommendations) instead
   of a flat default, and let hardening target a label selector, not only whole namespaces,
   so it can be rolled out gradually.

## Feedback on the brief

- **"Prevent ... from exchanging any network traffic"** is stronger than NetworkPolicy can
  promise. Host-network pods, NodePort or LoadBalancer paths with source rewriting, relays
  and already-open connections all fall outside it. I took it as direct pod-to-pod traffic
  at L3/L4 and listed the rest under limitations. It might help to say which of these the
  brief expects to be handled.
- **"Restore the workloads' previous connectivity"** is ambiguous when other things change
  during isolation, such as new policies or relabelled pods. I took it as "remove exactly
  what the tool added, and refuse to start if that wouldn't restore the old state".
- **Existing NetworkPolicies aren't mentioned**, but they're the biggest trap. Because
  policies combine with OR, the obvious implementation widens access for any target that
  already has a policy. Calling this out, or leaving it as a deliberate hidden test, are
  both fair. It decides whether a solution is safe.
- **"Namespace(s)" in the isolation story vs one namespace per side in the example.** I
  supported one namespace per side and would ask which was meant.
- **"Running without resource requests and limits"** reads as if both should always be set.
  I deliberately set no CPU limit (see Resource defaults); a hint that this is open would
  help.
- **The 4–6 hour estimate** fits the core logic. With the deployment manifests and RBAC, a
  runnable verification, tests that can be explained line by line, and the README, it's
  tight. The open trigger mechanism also adds design time.
