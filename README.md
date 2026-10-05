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
  combined with OR. See [Existing NetworkPolicies](#existing-networkpolicies-are-refused-not-merged).
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
- `skipped` lists workloads it won't patch, with the reason: bare Pods, Jobs, CronJobs, or
  anything labeled `workloadguard.io/ignore=true`.
- `unchanged` lists workloads that already meet the level.
- `notes` on a workload say what was deliberately left alone, or what the level might break.

The apply response is the same structure with `dryRun: false`, and each workload gets:

- `result`: `patched` or `failed`, with an `error`. A failure doesn't stop the other workloads.
- `rollout`: `ready`, or what went wrong, for example
  `rolled out, but pods are failing: pod unhardened-54bf… container web: Error (exit 1)`.

Apply works the plan out again from the live objects. It reports what it actually changed,
even if something changed since the dry run.

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

## Decisions

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
one workload from two peers at once. The production answer is
[AdminNetworkPolicy](https://network-policy-api.sigs.k8s.io/) or CNI-specific policies
(Calico, Cilium). Those have real deny rules that take priority over NetworkPolicies
instead of being combined with them.

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
| Pod with an owner (ReplicaSet etc.) | Ignored | Its controller is patched instead; editing the pod would be reverted. |
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

#### Undo (designed, not built)

Every patched workload records what was changed, in annotations:

- `workloadguard.io/hardening-changes`: a JSON list of `{container, field, before, after}`.
  Later runs add to it, for example baseline and then strict.
- `workloadguard.io/hardened-at` and `workloadguard.io/hardening-level`.

An undo endpoint, `POST /v1/hardening/undo` with the same body, would:

1. Read the annotation, and for each change check that the field **still holds the value the
   tool set**. If someone has changed it since, leave it and report it. Undo must not revert
   someone else's later edit.
2. Build a patch that removes the remaining fields. `before` is always `null`, so undo means
   deleting the field. Send it with the `resourceVersion`, like apply.
3. Remove the annotations, and offer the same plan/apply split as hardening.

One caveat: if the workload is managed by GitOps (Argo CD, Flux), the controller reverts the
patch on its next sync anyway. Hardening and undo there are only a stopgap; the real fix
belongs in the source manifests.
