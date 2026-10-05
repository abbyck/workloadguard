# Notes on the assignment brief

Points in the brief that were unclear or open to more than one reading, and how this
project interprets each. Links point to the relevant sections of the [README](../README.md).

- **"Prevent ... from exchanging any network traffic"** is stronger than NetworkPolicy can
  promise. Host-network pods, NodePort or LoadBalancer paths with source rewriting, relays
  and already-open connections all fall outside it. Interpreted as direct pod-to-pod
  traffic at L3/L4; the rest is listed under
  [known limitations](../README.md#known-limitations). It would help to say which of these
  are expected to be handled.
- **"Restore the workloads' previous connectivity"** is ambiguous when other things change
  during isolation, such as new policies or relabelled pods. Interpreted as "remove exactly
  what the tool added, and refuse to start if that wouldn't restore the old state".
- **Existing NetworkPolicies aren't mentioned**, but they're the biggest trap. Because
  policies combine with OR, the obvious implementation widens access for any target that
  already has a policy. Calling this out, or leaving it as a deliberate hidden test, are
  both fair; either way it decides whether a solution is safe.
- **"Namespace(s)" in the isolation story vs one namespace per side in the example.** Each
  side has one namespace here; it's unclear whether a side spanning several namespaces was
  meant.
- **Workloads by selector, or whole tenants?** The example picks pods by label, but the use
  case is containing a compromise "between tenants", and a tenant is usually a namespace.
  Both are supported: selectors as in the example, and whole namespaces with an explicit
  `allPods: true`.
- **"Think about how it behaves over time": one-shot or continuous?** Both tasks say "on
  demand", which suggests one-shot actions, but "over time" could also mean the tool should
  keep enforcing: noticing a deleted policy or a new one that reopens traffic. The tool
  uses one-shot actions on cluster-native state; the README explains what that covers and
  what it doesn't
  ([one-shot actions](../README.md#one-shot-actions-and-how-the-tool-behaves-over-time)).
- **"See what would change before it changes"** doesn't say whether apply must do exactly
  what the dry run showed. Here apply works the plan out again from the live objects and
  reports any difference; pinning apply to the reviewed plan is listed under
  [future work](../README.md#future-work).
- **"Running without resource requests and limits"** reads as if both should always be set.
  No CPU limit is set, deliberately
  ([resource defaults](../README.md#resource-defaults-set-them-conservatively)); a hint
  that this is open would help.
- **The 4–6 hour estimate** fits the core logic. With deployment manifests and RBAC, a
  runnable verification, tests that can be explained line by line, and the README, it's
  tight. The open trigger mechanism also adds design time.
