package guard

import (
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func deployment(ns, name string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

func TestNewAddsOwnNamespace(t *testing.T) {
	g := New(DefaultProtectedNamespaces, "workloadguard")
	if !slices.Contains(g.Protected(), "workloadguard") {
		t.Errorf("own namespace missing from %v", g.Protected())
	}
	// Listing it twice must not duplicate it.
	if got := New([]string{"workloadguard"}, "workloadguard").Protected(); len(got) != 1 {
		t.Errorf("protected = %v, want exactly [workloadguard]", got)
	}
	// Outside the cluster there's no own namespace; the list stays as given.
	if got := New([]string{"kube-system"}, "").Protected(); !slices.Equal(got, []string{"kube-system"}) {
		t.Errorf("protected = %v, want [kube-system]", got)
	}
}

func TestCheckNamespace(t *testing.T) {
	g := New(DefaultProtectedNamespaces, "workloadguard")

	tests := []struct {
		name          string
		ns            *corev1.Namespace
		wantProtected bool
	}{
		{"kube-system", namespace("kube-system", nil), true},
		{"kind storage", namespace("local-path-storage", nil), true},
		{"tool's own namespace", namespace("workloadguard", nil), true},
		{"opted out by label", namespace("tenant-a", map[string]string{IgnoreLabel: "true"}), true},
		// Only the exact value "true" opts out; anything else is a no-op, not a protection.
		{"label false", namespace("tenant-a", map[string]string{IgnoreLabel: "false"}), false},
		{"label typo", namespace("tenant-a", map[string]string{IgnoreLabel: "yes"}), false},
		{"ordinary namespace", namespace("tenant-a", nil), false},
		{"default is not protected", namespace("default", nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := g.CheckNamespace(tt.ns)
			assertProtected(t, err, tt.wantProtected)
		})
	}
}

func TestCheckWorkload(t *testing.T) {
	g := New(DefaultProtectedNamespaces, "workloadguard")

	tests := []struct {
		name          string
		obj           *appsv1.Deployment
		wantProtected bool
	}{
		{"opted out by label", deployment("legacy", "db", map[string]string{IgnoreLabel: "true"}), true},
		// Safety net: even if a caller skipped CheckNamespace, system workloads stay untouched.
		{"in kube-system", deployment("kube-system", "coredns", nil), true},
		{"workloadguard itself", deployment("workloadguard", "workloadguard", nil), true},
		{"label false", deployment("legacy", "db", map[string]string{IgnoreLabel: "false"}), false},
		{"ordinary workload", deployment("legacy", "unhardened", map[string]string{"app": "unhardened"}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := g.CheckWorkload("Deployment", tt.obj)
			assertProtected(t, err, tt.wantProtected)
		})
	}
}

func assertProtected(t *testing.T, err error, want bool) {
	t.Helper()
	var pe *ProtectedError
	switch {
	case want && !errors.As(err, &pe):
		t.Fatalf("want *ProtectedError, got %v", err)
	case want && pe.Reason == "":
		t.Errorf("ProtectedError has no reason: %v", pe)
	case !want && err != nil:
		t.Errorf("want nil, got %v", err)
	}
}
