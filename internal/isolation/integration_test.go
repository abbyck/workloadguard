//go:build integration

package isolation

// Integration tests against a real API server (the kind cluster). They cover what the fake
// clientset can't: real server-side apply and real label-selector queries. Traffic
// enforcement itself is proved by hack/verify.sh.
//
//   go test -tags integration ./internal/...      (or: make integration)

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/abbyck/workloadguard/internal/guard"
	"github.com/abbyck/workloadguard/internal/kube"
)

func integrationClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	kctx := os.Getenv("WG_KUBE_CONTEXT")
	if kctx == "" {
		kctx = "kind-workloadguard"
	}
	client, err := kube.NewClient(kube.Options{Context: kctx})
	if err != nil {
		t.Skipf("no cluster (%v)", err)
	}
	if _, err := client.Discovery().ServerVersion(); err != nil {
		t.Skipf("cluster %s unreachable: %v", kctx, err)
	}
	return client
}

// tempNamespace creates a namespace that is deleted when the test ends.
func tempNamespace(t *testing.T, client kubernetes.Interface, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("wg-it-%s-%04x", prefix, rand.IntN(0xffff))
	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{}) })
	return name
}

func TestIntegrationIsolationLifecycle(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	nsA, nsB := tempNamespace(t, client, "a"), tempNamespace(t, client, "b")
	svc := NewService(client, guard.New(guard.DefaultProtectedNamespaces, ""), []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16")})
	req := Request{A: Side{Namespace: nsA, Selector: map[string]string{"app": "a"}}, B: Side{Namespace: nsB, Selector: map[string]string{"app": "b"}}}

	iso, created, err := svc.On(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(iso.Policies) != 2 {
		t.Fatalf("on: created=%v %+v", created, iso)
	}
	// No pods match yet: allowed, with a warning per side.
	if len(iso.Warnings) != 2 {
		t.Errorf("warnings = %v, want one per side", iso.Warnings)
	}

	// The API server stored what Build produced, and it is owned by our field manager.
	np, err := client.NetworkingV1().NetworkPolicies(nsA).Get(ctx, PolicyName(iso.ID, "a"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.PolicyTypes) != 2 || np.Spec.Ingress[0].From[len(np.Spec.Ingress[0].From)-2].IPBlock.Except[0] != "10.244.0.0/16" {
		t.Errorf("stored policy = %+v", np.Spec)
	}
	owned := false
	for _, mf := range np.ManagedFields {
		owned = owned || mf.Manager == fieldManager
	}
	if !owned {
		t.Error("policy not owned by the workloadguard field manager")
	}

	// Re-applying through real server-side apply changes nothing.
	if _, created, err := svc.On(ctx, req); err != nil || created {
		t.Errorf("re-apply: created=%v err=%v", created, err)
	}

	// A policy someone else adds on a target makes a second, different isolation conflict.
	other := Request{A: req.A, B: Side{Namespace: nsB, Selector: map[string]string{"app": "c"}}}
	if _, _, err := svc.On(ctx, other); !errors.As(err, new(*ConflictError)) {
		t.Errorf("stacked isolation: want ConflictError, got %v", err)
	}

	deleted, err := svc.Off(ctx, iso.ID)
	if err != nil || len(deleted) != 2 {
		t.Fatalf("off: %v %v", deleted, err)
	}
	left, err := client.NetworkingV1().NetworkPolicies(nsA).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Errorf("policies left in %s: %d", nsA, len(left.Items))
	}
}

func TestIntegrationWholeNamespaces(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	nsA, nsB := tempNamespace(t, client, "wa"), tempNamespace(t, client, "wb")
	svc := NewService(client, guard.New(guard.DefaultProtectedNamespaces, ""), []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16")})
	req := Request{A: Side{Namespace: nsA, AllPods: true}, B: Side{Namespace: nsB, AllPods: true}}

	iso, created, err := svc.On(ctx, req)
	if err != nil || !created {
		t.Fatalf("on: created=%v err=%v", created, err)
	}
	t.Cleanup(func() { _, _ = svc.Off(context.Background(), iso.ID) })

	np, err := client.NetworkingV1().NetworkPolicies(nsA).Get(ctx, PolicyName(iso.ID, "a"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// As stored by the API server: every pod selected, and only three peers (every other
	// namespace, then the two ipBlocks): no rule admits any pod of the peer's namespace.
	if sel := np.Spec.PodSelector; len(sel.MatchLabels) != 0 || len(sel.MatchExpressions) != 0 {
		t.Errorf("podSelector = %+v, want empty", sel)
	}
	if peers := np.Spec.Ingress[0].From; len(peers) != 3 || peers[0].NamespaceSelector == nil || peers[1].IPBlock == nil || peers[2].IPBlock == nil {
		t.Errorf("peers = %+v, want namespaceSelector + 2 ipBlocks", peers)
	}

	// The isolation reads back as whole namespaces.
	got, err := svc.Get(ctx, iso.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.A.AllPods || !got.B.AllPods {
		t.Errorf("get = %+v, want allPods on both sides", got)
	}

	if deleted, err := svc.Off(ctx, iso.ID); err != nil || len(deleted) != 2 {
		t.Fatalf("off: %v %v", deleted, err)
	}
}
