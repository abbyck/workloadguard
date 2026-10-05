package isolation

import (
	"context"
	"errors"
	"testing"

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
