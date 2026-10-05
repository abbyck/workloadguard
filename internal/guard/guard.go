// Package guard decides which namespaces and workloads workloadguard must never touch.
// Isolation and hardening both go through it, so the rules can't drift apart.
package guard

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IgnoreLabel opts a namespace or workload out of workloadguard when set to "true".
// Any other value is treated as not set, so a typo never silently protects something.
const IgnoreLabel = "workloadguard.io/ignore"

// DefaultProtectedNamespaces are the cluster's own namespaces. Isolating or patching
// anything in them can break the cluster itself (DNS, CNI, storage provisioning).
var DefaultProtectedNamespaces = []string{
	"kube-system",
	"kube-public",
	"kube-node-lease",
	"local-path-storage", // kind's storage provisioner
}

// ProtectedError says why a target was refused. The API maps it to 403.
type ProtectedError struct {
	Kind      string // "Namespace", "Deployment", ...
	Namespace string
	Name      string
	Reason    string
}

func (e *ProtectedError) Error() string {
	if e.Kind == "Namespace" {
		return fmt.Sprintf("namespace %q is protected: %s", e.Name, e.Reason)
	}
	return fmt.Sprintf("%s %s/%s is protected: %s", e.Kind, e.Namespace, e.Name, e.Reason)
}

// Guard holds the protected namespace list.
type Guard struct {
	protected []string
}

// New returns a Guard protecting the given namespaces plus ownNamespace (where
// workloadguard itself runs), so the tool can't isolate or patch itself. ownNamespace
// may be empty when running outside the cluster.
func New(protected []string, ownNamespace string) *Guard {
	p := slices.Clone(protected)
	if ownNamespace != "" && !slices.Contains(p, ownNamespace) {
		p = append(p, ownNamespace)
	}
	slices.Sort(p)
	return &Guard{protected: p}
}

// Protected returns the protected namespace names, sorted.
func (g *Guard) Protected() []string {
	return slices.Clone(g.protected)
}

// CheckNamespace returns a *ProtectedError if ns must not be touched, or nil.
func (g *Guard) CheckNamespace(ns *corev1.Namespace) error {
	if slices.Contains(g.protected, ns.Name) {
		return &ProtectedError{Kind: "Namespace", Name: ns.Name, Reason: "in the protected namespace list"}
	}
	if ns.Labels[IgnoreLabel] == "true" {
		return &ProtectedError{Kind: "Namespace", Name: ns.Name, Reason: "labeled " + IgnoreLabel + "=true"}
	}
	return nil
}

// CheckWorkload returns a *ProtectedError if the workload must not be touched, or nil.
// It doesn't look at the namespace's labels; callers check the namespace first with
// CheckNamespace. The namespace name is checked again here as a safety net.
func (g *Guard) CheckWorkload(kind string, obj metav1.Object) error {
	refuse := func(reason string) error {
		return &ProtectedError{Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), Reason: reason}
	}
	if slices.Contains(g.protected, obj.GetNamespace()) {
		return refuse("in protected namespace " + obj.GetNamespace())
	}
	if obj.GetLabels()[IgnoreLabel] == "true" {
		return refuse("labeled " + IgnoreLabel + "=true")
	}
	return nil
}
