package isolation

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/abbyck/workloadguard/internal/guard"
)

func side(ns string, kv ...string) Side {
	sel := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		sel[kv[i]] = kv[i+1]
	}
	return Side{Namespace: ns, Selector: sel}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     Request
		wantErr string // substring of one problem; empty means valid
	}{
		{
			name: "brief's example",
			req:  Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")},
		},
		{
			name:    "missing namespace",
			req:     Request{A: side("", "app", "gateway"), B: side("tenant-b", "app", "dashboard")},
			wantErr: "a.namespace is required",
		},
		{
			name:    "invalid namespace name",
			req:     Request{A: side("Tenant_A", "app", "gateway"), B: side("tenant-b", "app", "dashboard")},
			wantErr: `a.namespace "Tenant_A"`,
		},
		{
			// An empty selector would select every pod in tenant-b.
			name:    "empty selector",
			req:     Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b")},
			wantErr: "b.selector must have at least one label",
		},
		{
			name:    "invalid label key",
			req:     Request{A: side("tenant-a", "app/x/y", "gateway"), B: side("tenant-b", "app", "dashboard")},
			wantErr: `a.selector key "app/x/y"`,
		},
		{
			name:    "invalid label value",
			req:     Request{A: side("tenant-a", "app", "not valid!"), B: side("tenant-b", "app", "dashboard")},
			wantErr: "a.selector[app]",
		},
		{
			name:    "a equals b",
			req:     Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-a", "app", "gateway")},
			wantErr: "can match the same pods",
		},
		{
			// The subtle case: different selectors, but a pod labeled app=gateway,tier=edge
			// matches both, so it would have to be isolated from itself.
			name:    "same namespace, selectors without a conflicting key",
			req:     Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-a", "tier", "edge")},
			wantErr: "can match the same pods",
		},
		{
			name:    "same namespace, one selector a subset of the other",
			req:     Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-a", "app", "gateway", "tier", "edge")},
			wantErr: "can match the same pods",
		},
		{
			name: "same namespace, conflicting values never overlap",
			req:  Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-a", "app", "other")},
		},
		{
			name: "whole namespaces, asked for explicitly",
			req:  Request{A: Side{Namespace: "tenant-a", AllPods: true}, B: Side{Namespace: "tenant-b", AllPods: true}},
		},
		{
			name: "whole namespace against a selector",
			req:  Request{A: Side{Namespace: "tenant-a", AllPods: true}, B: side("tenant-b", "app", "dashboard")},
		},
		{
			// Ambiguous: does the operator mean the selector or the whole namespace?
			name:    "allPods and a selector together",
			req:     Request{A: Side{Namespace: "tenant-a", AllPods: true, Selector: map[string]string{"app": "gateway"}}, B: side("tenant-b", "app", "dashboard")},
			wantErr: "a: set either selector or allPods, not both",
		},
		{
			// An empty selector still isn't read as "every pod"; the error says how to ask.
			name:    "empty selector points at allPods",
			req:     Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b")},
			wantErr: "set allPods: true instead",
		},
		{
			// A whole namespace overlaps anything else in it, including itself.
			name:    "whole namespace against a selector in the same namespace",
			req:     Request{A: Side{Namespace: "tenant-a", AllPods: true}, B: side("tenant-a", "app", "gateway")},
			wantErr: "can match the same pods",
		},
		{
			name: "same selector in different namespaces is fine",
			req:  Request{A: side("tenant-a", "app", "web"), B: side("tenant-b", "app", "web")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var inv *InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("want *InvalidError, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	err := Request{A: side(""), B: side("")}.Validate()
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("want *InvalidError, got %v", err)
	}
	// Both namespaces and both selectors: the operator sees everything at once.
	if len(inv.Problems) != 4 {
		t.Errorf("got %d problems, want 4: %v", len(inv.Problems), inv.Problems)
	}
}

func ns(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func pod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels}}
}

func TestCheck(t *testing.T) {
	g := guard.New(guard.DefaultProtectedNamespaces, "workloadguard")
	brief := Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")}
	baseCluster := []runtime.Object{
		ns("tenant-a", nil), ns("tenant-b", nil),
		pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}),
		pod("tenant-b", "dashboard-1", map[string]string{"app": "dashboard"}),
	}

	tests := []struct {
		name         string
		objects      []runtime.Object
		req          Request
		wantWarnings int
		check        func(t *testing.T, err error)
	}{
		{
			name:    "valid, both sides match pods",
			objects: baseCluster,
			req:     brief,
		},
		{
			name:    "missing namespace",
			objects: []runtime.Object{ns("tenant-a", nil)},
			req:     brief,
			check: func(t *testing.T, err error) {
				var nf *NamespaceNotFoundError
				if !errors.As(err, &nf) || nf.Namespace != "tenant-b" {
					t.Errorf("want NamespaceNotFoundError for tenant-b, got %v", err)
				}
			},
		},
		{
			name:    "protected namespace refused",
			objects: append(baseCluster, ns("kube-system", nil)),
			req:     Request{A: side("tenant-a", "app", "gateway"), B: side("kube-system", "k8s-app", "kube-dns")},
			check:   wantProtected,
		},
		{
			name:    "namespace opted out by label",
			objects: []runtime.Object{ns("tenant-a", nil), ns("tenant-b", map[string]string{guard.IgnoreLabel: "true"})},
			req:     brief,
			check:   wantProtected,
		},
		{
			name: "matching pod opted out by label",
			objects: []runtime.Object{
				ns("tenant-a", nil), ns("tenant-b", nil),
				pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}),
				pod("tenant-b", "dashboard-1", map[string]string{"app": "dashboard", guard.IgnoreLabel: "true"}),
			},
			req:   brief,
			check: wantProtected,
		},
		{
			// Only pods the selector matches count; an opted-out neighbour doesn't block isolation.
			name: "unrelated opted-out pod is ignored",
			objects: append(baseCluster,
				pod("tenant-b", "db-1", map[string]string{"app": "db", guard.IgnoreLabel: "true"})),
			req: brief,
		},
		{
			// NetworkPolicy doesn't apply to host-network pods; isolating one would silently
			// do nothing, so it's refused instead of reported as done.
			name: "host-network pod on a side is refused",
			objects: []runtime.Object{
				ns("tenant-a", nil), ns("tenant-b", nil),
				pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}),
				func() runtime.Object {
					p := pod("tenant-b", "dashboard-1", map[string]string{"app": "dashboard"})
					p.Spec.HostNetwork = true
					return p
				}(),
			},
			req: brief,
			check: func(t *testing.T, err error) {
				var un *UnsupportedError
				if !errors.As(err, &un) || !strings.Contains(un.Reason, "hostNetwork") {
					t.Errorf("want UnsupportedError about hostNetwork, got %v", err)
				}
			},
		},
		{
			// Isolating a whole namespace touches every pod in it, opted-out ones included.
			name: "whole namespace with an opted-out pod is refused",
			objects: append(baseCluster,
				pod("tenant-b", "db-1", map[string]string{"app": "db", guard.IgnoreLabel: "true"})),
			req:   Request{A: brief.A, B: Side{Namespace: "tenant-b", AllPods: true}},
			check: wantProtected,
		},
		{
			name:         "empty namespace is a warning, not an error",
			objects:      []runtime.Object{ns("tenant-a", nil), ns("tenant-b", nil), pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"})},
			req:          Request{A: brief.A, B: Side{Namespace: "tenant-b", AllPods: true}},
			wantWarnings: 1,
		},
		{
			name:         "selector matching no pods is a warning, not an error",
			objects:      []runtime.Object{ns("tenant-a", nil), ns("tenant-b", nil), pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"})},
			req:          brief,
			wantWarnings: 1,
		},
		{
			name:    "invalid request fails before touching the cluster",
			objects: nil,
			req:     Request{A: side("tenant-a"), B: side("tenant-b", "app", "dashboard")},
			check: func(t *testing.T, err error) {
				var inv *InvalidError
				if !errors.As(err, &inv) {
					t.Errorf("want *InvalidError, got %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objects...)
			warnings, err := Check(context.Background(), client, g, tt.req)
			if tt.check != nil {
				tt.check(t, err)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(warnings) != tt.wantWarnings {
				t.Errorf("warnings = %v, want %d", warnings, tt.wantWarnings)
			}
		})
	}
}

func wantProtected(t *testing.T, err error) {
	t.Helper()
	var pe *guard.ProtectedError
	if !errors.As(err, &pe) {
		t.Errorf("want *guard.ProtectedError, got %v", err)
	}
}
