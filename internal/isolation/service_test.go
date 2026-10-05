package isolation

import (
	"context"
	"errors"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abbyck/workloadguard/internal/guard"
)

var brief = Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")}

func newCluster() *fake.Clientset {
	return fake.NewClientset(
		ns("tenant-a", nil), ns("tenant-b", nil),
		pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}),
		pod("tenant-b", "dashboard-1", map[string]string{"app": "dashboard"}),
		node("kind-control-plane", "10.244.0.0/24"),
	)
}

func newService(client *fake.Clientset) *Service {
	return NewService(client, guard.New(guard.DefaultProtectedNamespaces, ""), kindPodCIDRs)
}

// managedPolicies counts workloadguard's policies across the cluster.
func managedPolicies(t *testing.T, client *fake.Clientset) int {
	t.Helper()
	list, err := client.NetworkingV1().NetworkPolicies("").List(context.Background(), metav1.ListOptions{
		LabelSelector: ManagedByLabel + "=" + ManagedByValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func TestOnCreatesBothPolicies(t *testing.T) {
	client := newCluster()
	iso, created, err := newService(client).On(context.Background(), brief)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("created = false on first call")
	}
	if iso.ID != brief.ID() || len(iso.Policies) != 2 {
		t.Errorf("isolation = %+v", iso)
	}
	if n := managedPolicies(t, client); n != 2 {
		t.Errorf("cluster has %d managed policies, want 2", n)
	}
}

func TestOnIsIdempotent(t *testing.T) {
	client := newCluster()
	svc := newService(client)
	if _, _, err := svc.On(context.Background(), brief); err != nil {
		t.Fatal(err)
	}
	// Same request with sides swapped: same isolation, nothing new.
	_, created, err := svc.On(context.Background(), Request{A: brief.B, B: brief.A})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created = true when both policies already existed")
	}
	if n := managedPolicies(t, client); n != 2 {
		t.Errorf("cluster has %d managed policies after re-apply, want 2", n)
	}
}

// failApplyIn makes server-side apply of NetworkPolicies fail in one namespace.
func failApplyIn(client *fake.Clientset, namespace string) {
	client.PrependReactor("patch", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == namespace {
			return true, nil, errors.New("simulated API failure")
		}
		return false, nil, nil
	})
}

func TestOnRollsBackWhenSecondPolicyFails(t *testing.T) {
	client := newCluster()
	failApplyIn(client, "tenant-b")

	_, _, err := newService(client).On(context.Background(), brief)
	if err == nil {
		t.Fatal("want error when the second policy fails")
	}
	// A one-sided isolation nobody asked for must not be left behind.
	if n := managedPolicies(t, client); n != 0 {
		t.Errorf("cluster has %d managed policies after a failed request, want 0", n)
	}
}

func TestRollbackKeepsPoliciesThatAlreadyExisted(t *testing.T) {
	client := newCluster()
	svc := newService(client)
	if _, _, err := svc.On(context.Background(), brief); err != nil {
		t.Fatal(err)
	}
	failApplyIn(client, "tenant-b")

	// A retry that fails must not tear down the isolation that was already in place.
	if _, _, err := svc.On(context.Background(), brief); err == nil {
		t.Fatal("want error")
	}
	if n := managedPolicies(t, client); n != 2 {
		t.Errorf("cluster has %d managed policies, want the original 2", n)
	}
}

func TestOnRefusesWhenPodCIDRsDontCoverNodes(t *testing.T) {
	client := newCluster()
	if _, err := client.CoreV1().Nodes().Create(context.Background(), node("w9", "10.99.0.0/24"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _, err := newService(client).On(context.Background(), brief)
	var cidrErr *PodCIDRError
	if !errors.As(err, &cidrErr) {
		t.Fatalf("want PodCIDRError, got %v", err)
	}
	if n := managedPolicies(t, client); n != 0 {
		t.Errorf("cluster has %d managed policies, want 0", n)
	}
}

func policy(namespace, name string, sel metav1.LabelSelector, lbls map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: lbls},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: sel},
	}
}

func matchLabels(kv ...string) metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: side("", kv...).Selector}
}

func TestOnRefusesExistingPolicies(t *testing.T) {
	tests := []struct {
		name         string
		existing     *networkingv1.NetworkPolicy
		wantConflict bool
	}{
		{
			// examples/extras/preexisting-netpol.yaml: dashboard only accepts the bystander.
			// Adding "everything except gateway" would open dashboard to every namespace.
			name:         "restrictive policy on a target",
			existing:     policy("tenant-b", "dashboard-allow-bystander-only", matchLabels("app", "dashboard"), nil),
			wantConflict: true,
		},
		{
			name:         "namespace-wide default deny (empty podSelector)",
			existing:     policy("tenant-a", "default-deny", metav1.LabelSelector{}, nil),
			wantConflict: true,
		},
		{
			name: "matchExpressions selecting the target",
			existing: policy("tenant-a", "web-tier", metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"gateway", "api"}},
			}}, nil),
			wantConflict: true,
		},
		{
			// Two "everything except X" policies on the same pods add up to "everything".
			name: "another workloadguard isolation on the same pods",
			existing: policy("tenant-a", "workloadguard-iso-other-a", matchLabels("app", "gateway"),
				map[string]string{ManagedByLabel: ManagedByValue, IDLabel: "iso-other"}),
			wantConflict: true,
		},
		{
			name:     "policy for other pods in the same namespace",
			existing: policy("tenant-b", "db-only", matchLabels("app", "db"), nil),
		},
		{
			name:     "policy in an unrelated namespace",
			existing: policy("shared", "default-deny", metav1.LabelSelector{}, nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newCluster()
			if _, err := client.NetworkingV1().NetworkPolicies(tt.existing.Namespace).Create(
				context.Background(), tt.existing, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			_, _, err := newService(client).On(context.Background(), brief)

			var ce *ConflictError
			if tt.wantConflict != errors.As(err, &ce) {
				t.Fatalf("err = %v, want ConflictError: %v", err, tt.wantConflict)
			}
			if !tt.wantConflict && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantConflict {
				if len(ce.Policies) != 1 || ce.Policies[0].Name != tt.existing.Name {
					t.Errorf("conflicts = %+v, want only %s", ce.Policies, tt.existing.Name)
				}
				// The refused request must not have created anything; only the pre-existing
				// policy may be managed (when it's another isolation's).
				wantManaged := 0
				if tt.existing.Labels[ManagedByLabel] != "" {
					wantManaged = 1
				}
				if n := managedPolicies(t, client); n != wantManaged {
					t.Errorf("cluster has %d managed policies, want %d", n, wantManaged)
				}
			}
		})
	}
}

func TestWholeNamespaceConflictsWithEveryPolicyInIt(t *testing.T) {
	// A policy for app=db in tenant-b doesn't touch the dashboard, so it doesn't block
	// isolating the dashboard. It does block isolating all of tenant-b: the isolation would
	// also select the db pods, now or once they start, and the two policies would combine.
	dbPolicy := policy("tenant-b", "db-only", matchLabels("app", "db"), nil)

	client := newCluster()
	if err := client.Tracker().Add(dbPolicy); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newService(client).On(context.Background(), brief); err != nil {
		t.Fatalf("selector isolation: %v", err)
	}

	client = newCluster()
	if err := client.Tracker().Add(dbPolicy); err != nil {
		t.Fatal(err)
	}
	_, _, err := newService(client).On(context.Background(), Request{A: brief.A, B: Side{Namespace: "tenant-b", AllPods: true}})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Policies[0].Name != "db-only" {
		t.Fatalf("whole-namespace isolation: want ConflictError naming db-only, got %v", err)
	}
}

func TestConflictCatchesPolicyBeforePodsExist(t *testing.T) {
	// No dashboard pod is running, but the policy would select one as soon as it starts.
	client := fake.NewClientset(
		ns("tenant-a", nil), ns("tenant-b", nil), node("cp", "10.244.0.0/24"),
		pod("tenant-a", "gateway-1", map[string]string{"app": "gateway"}),
		policy("tenant-b", "dashboard-policy", matchLabels("app", "dashboard"), nil),
	)
	_, _, err := newService(client).On(context.Background(), brief)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want ConflictError, got %v", err)
	}
}
