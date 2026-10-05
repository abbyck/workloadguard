package isolation

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func mustOn(t *testing.T, svc *Service, r Request) *Isolation {
	t.Helper()
	iso, _, err := svc.On(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return iso
}

func policyNames(t *testing.T, svc *Service) map[string]bool {
	t.Helper()
	list, err := svc.client.NetworkingV1().NetworkPolicies("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range list.Items {
		names[p.Namespace+"/"+p.Name] = true
	}
	return names
}

// Turning isolation off must delete exactly what it created. A broader delete (say, every
// workloadguard policy in the namespace, or every policy selecting the pods) would remove
// other people's policies or another isolation and change connectivity it doesn't own.
func TestOffRemovesOnlyItsOwnPolicies(t *testing.T) {
	client := newCluster()
	svc := newService(client)
	ctx := context.Background()

	iso := mustOn(t, svc, brief)
	// Created after isolation is on, so the conflict check doesn't refuse it: another team's
	// policy in tenant-a, and another isolation's policy in the same namespace.
	for _, p := range []runtime.Object{
		policy("tenant-a", "team-policy", matchLabels("app", "billing"), nil),
		policy("tenant-a", "workloadguard-iso-00000000ff-a", matchLabels("app", "billing"),
			map[string]string{ManagedByLabel: ManagedByValue, IDLabel: "iso-00000000ff"}),
	} {
		if err := client.Tracker().Add(p); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := svc.Off(ctx, iso.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Errorf("deleted %v, want this isolation's 2 policies", deleted)
	}
	names := policyNames(t, svc)
	for _, keep := range []string{"tenant-a/team-policy", "tenant-a/workloadguard-iso-00000000ff-a"} {
		if !names[keep] {
			t.Errorf("%s was deleted, but it doesn't belong to %s", keep, iso.ID)
		}
	}
	for _, ref := range iso.Policies {
		if names[ref.Namespace+"/"+ref.Name] {
			t.Errorf("%s/%s still exists after Off", ref.Namespace, ref.Name)
		}
	}
}

func TestOffIsIdempotent(t *testing.T) {
	svc := newService(newCluster())
	iso := mustOn(t, svc, brief)
	if _, err := svc.Off(context.Background(), iso.ID); err != nil {
		t.Fatal(err)
	}
	// Second call, e.g. a client retry after a lost response: success, nothing to delete.
	deleted, err := svc.Off(context.Background(), iso.ID)
	if err != nil {
		t.Fatalf("second Off: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("second Off deleted %v, want nothing", deleted)
	}
}

func TestOffRejectsMalformedID(t *testing.T) {
	client := newCluster()
	svc := newService(client)
	mustOn(t, svc, brief)
	// Would otherwise become the label selector "...isolation-id=x,app=gateway".
	_, err := svc.Off(context.Background(), "x,app=gateway")
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("want InvalidError, got %v", err)
	}
	if n := managedPolicies(t, client); n != 2 {
		t.Errorf("%d managed policies left, want 2", n)
	}
}

func TestOffContinuesPastFailures(t *testing.T) {
	client := newCluster()
	svc := newService(client)
	iso := mustOn(t, svc, brief)
	client.PrependReactor("delete", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "tenant-a" {
			return true, nil, errors.New("simulated API failure")
		}
		return false, nil, nil
	})

	deleted, err := svc.Off(context.Background(), iso.ID)
	if err == nil {
		t.Fatal("want error when a delete fails")
	}
	// tenant-b's policy is still removed, and the error says what's left.
	if len(deleted) != 1 || deleted[0].Namespace != "tenant-b" {
		t.Errorf("deleted = %v, want only tenant-b's policy", deleted)
	}
}

func TestGetAndList(t *testing.T) {
	client := newCluster()
	if err := client.Tracker().Add(pod("tenant-b", "billing-1", map[string]string{"app": "billing"})); err != nil {
		t.Fatal(err)
	}
	svc := newService(client)
	first := mustOn(t, svc, brief)
	second := mustOn(t, svc, Request{A: side("tenant-a", "app", "api"), B: side("tenant-b", "app", "billing")})

	list, err := svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, iso := range list {
		ids[iso.ID] = true
		if len(iso.Policies) != 2 || len(iso.Warnings) != 0 {
			t.Errorf("isolation %s: %d policies, warnings %v", iso.ID, len(iso.Policies), iso.Warnings)
		}
	}
	if len(list) != 2 || !ids[first.ID] || !ids[second.ID] {
		t.Errorf("list = %v, want %s and %s", ids, first.ID, second.ID)
	}

	got, err := svc.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.A.String() != first.A.String() || got.B.String() != first.B.String() {
		t.Errorf("get returned %+v, want %+v", got, first)
	}

	_, err = svc.Get(context.Background(), "iso-0000000000")
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("want NotFoundError for unknown ID, got %v", err)
	}
}

func TestGetFlagsIncompleteIsolation(t *testing.T) {
	client := newCluster()
	svc := newService(client)
	iso := mustOn(t, svc, brief)
	// Someone deletes one side's policy by hand.
	if err := client.NetworkingV1().NetworkPolicies("tenant-b").Delete(context.Background(),
		PolicyName(iso.ID, "b"), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), iso.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 {
		t.Errorf("warnings = %v, want one 'incomplete' warning", got.Warnings)
	}
}
