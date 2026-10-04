# workloadguard

A small Go service that runs inside a Kubernetes cluster and does two things on request:

- **Isolate:** blocks all traffic between two workloads (each picked by namespace and label
  selector) using NetworkPolicies. Their other traffic keeps working, and turning isolation
  off restores the connectivity they had before.
- **Harden:** finds workloads in one or more namespaces that have no resource requests/limits
  or no hardened `securityContext`, and patches them. A dry run shows the changes first.

Everything it creates is labeled `app.kubernetes.io/managed-by=workloadguard`, so you can
inspect it with `kubectl` without the tool. It is built with plain client-go and tested on kind.
