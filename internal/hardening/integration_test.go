//go:build integration

package hardening

// Integration tests against a real API server (the kind cluster). They cover what the fake
// clientset can't: server-side dry run not persisting, resourceVersion conflicts, real
// strategic merge patches with server defaulting, and real rollouts.
//
//   go test -tags integration ./internal/...      (or: make integration)

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

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

// setup creates a throwaway namespace with an unhardened nginx Deployment and waits for it.
func setup(t *testing.T, client kubernetes.Interface) (string, *appsv1.Deployment) {
	t.Helper()
	ctx := context.Background()
	ns := fmt.Sprintf("wg-it-harden-%04x", rand.IntN(0xffff))
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				// Same image as the samples, already on the kind nodes. Runs as uid 101 on
				// 8080, so baseline hardening must keep it running.
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginxinc/nginx-unprivileged:1.27-alpine"}}},
			},
		},
	}
	created, err := client.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ns, created
}

func newIntegrationService(client kubernetes.Interface) *Service {
	return NewService(client, guard.New(guard.DefaultProtectedNamespaces, ""), testDefaults, 90*time.Second)
}

func TestIntegrationPlanApplyUndo(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns, original := setup(t, client)
	svc := newIntegrationService(client)
	req := Request{Namespaces: []string{ns}}

	// Plan sends a real server-side dry run. It must validate the patch and store nothing.
	plan, err := svc.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Workloads) != 1 || plan.Workloads[0].Error != "" {
		t.Fatalf("plan: %d workloads, error %q", len(plan.Workloads), plan.Workloads[0].Error)
	}
	after, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != original.Generation {
		t.Fatalf("dry run changed the deployment (generation %d -> %d)", original.Generation, after.Generation)
	}

	// Apply, and the pods must come back Ready with the hardened template.
	applied, err := svc.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if w := applied.Workloads[0]; w.Result != ResultPatched || w.Rollout != "ready" {
		t.Fatalf("apply = %+v", w)
	}
	again, err := svc.Apply(ctx, req)
	if err != nil || len(again.Workloads) != 0 {
		t.Errorf("re-apply: %+v %v, want no-op", again.Workloads, err)
	}

	// Undo returns the template to what the API server stored at creation, defaults and all.
	undone, err := svc.UndoApply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if w := undone.Workloads[0]; w.Result != ResultPatched || w.Rollout != "ready" {
		t.Fatalf("undo = %+v", w)
	}
	final, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final.Spec.Template.Spec, original.Spec.Template.Spec) {
		t.Errorf("template after undo differs from the original:\n got  %+v\n want %+v",
			final.Spec.Template.Spec, original.Spec.Template.Spec)
	}
}

func TestIntegrationStalePatchIsRejected(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns, original := setup(t, client)
	svc := newIntegrationService(client)
	k := patchable[0] // Deployment

	// Build a patch from the object as first read, then let someone else change it.
	_, _, patch, err := svc.buildPatch(k, original, Baseline, false)
	if err != nil {
		t.Fatal(err)
	}
	// The deployment controller is updating status at the same time, so this edit needs
	// the usual read-modify-write retry itself.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.Spec.Template.Spec.Containers[0].Image = "nginxinc/nginx-unprivileged:1.27"
		_, err = client.AppsV1().Deployments(ns).Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// The stale patch carries the old resourceVersion: the server must refuse it, so the
	// tool re-reads instead of overwriting the concurrent change.
	_, err = k.patch(ctx, client, ns, "web", patch, metav1.PatchOptions{FieldManager: fieldManager})
	if !apierrors.IsConflict(err) {
		t.Errorf("want a Conflict for a stale patch, got %v", err)
	}
}
