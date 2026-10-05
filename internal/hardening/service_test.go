package hardening

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/abbyck/workloadguard/internal/guard"
)

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func deployment(ns, name string, labels map[string]string, containers ...corev1.Container) *appsv1.Deployment {
	if len(containers) == 0 {
		containers = []corev1.Container{{Name: "web", Image: "nginx"}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels, ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: containers},
			},
		},
	}
}

func hardenedDeployment(ns, name string) *appsv1.Deployment {
	d := deployment(ns, name, nil, corev1.Container{
		Name: "web", Image: "nginx",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
	})
	d.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: "RuntimeDefault"}}
	return d
}

// newTestService returns a Service over a fake cluster. Server-side dry runs are
// intercepted and not stored, as the real API server does.
func newTestService(objs ...runtime.Object) (*Service, *fake.Clientset) {
	client := fake.NewClientset(objs...)
	client.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		if len(pa.PatchOptions.DryRun) == 0 {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
		return true, obj, err
	})
	svc := NewService(client, guard.New(guard.DefaultProtectedNamespaces, ""), testDefaults, 0)
	svc.settle = 0
	return svc, client
}

func getDeployment(t *testing.T, client *fake.Clientset, ns, name string) *appsv1.Deployment {
	t.Helper()
	d, err := client.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func names(refs []WorkloadRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func TestPlanChangesNothing(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	before := getDeployment(t, client, "legacy", "unhardened")

	plan, err := svc.Plan(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Level != Baseline {
		t.Errorf("plan dryRun=%v level=%s, want true/baseline", plan.DryRun, plan.Level)
	}
	if len(plan.Workloads) != 1 || len(plan.Workloads[0].Changes) == 0 || !plan.Workloads[0].RestartsPods {
		t.Fatalf("workloads = %+v", plan.Workloads)
	}
	if plan.Workloads[0].Result != "" {
		t.Errorf("plan reported a result %q", plan.Workloads[0].Result)
	}
	after := getDeployment(t, client, "legacy", "unhardened")
	if after.ResourceVersion != before.ResourceVersion || after.Annotations[HardenedAtAnnotation] != "" {
		t.Error("plan modified the deployment")
	}
}

func TestApplyMatchesPlanAndIsIdempotent(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	ctx := context.Background()
	req := Request{Namespaces: []string{"legacy"}}

	plan, err := svc.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := svc.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Workloads) != 1 || applied.Workloads[0].Result != ResultPatched {
		t.Fatalf("apply = %+v", applied.Workloads)
	}
	// The plan said what would change; apply must have changed exactly that.
	if got, want := fields(applied.Workloads[0].Changes), fields(plan.Workloads[0].Changes); len(got) != len(want) {
		t.Errorf("apply changed %v, plan said %v", got, want)
	}

	d := getDeployment(t, client, "legacy", "unhardened")
	c := d.Spec.Template.Spec.Containers[0]
	if c.SecurityContext == nil || !ptr.Equal(c.SecurityContext.AllowPrivilegeEscalation, ptr.To(false)) {
		t.Errorf("container securityContext = %+v", c.SecurityContext)
	}
	if c.Image != "nginx" {
		t.Errorf("patch clobbered the image: %q", c.Image)
	}
	var recorded []Change
	if err := json.Unmarshal([]byte(d.Annotations[ChangesAnnotation]), &recorded); err != nil || len(recorded) != len(applied.Workloads[0].Changes) {
		t.Errorf("changes annotation = %q (%v)", d.Annotations[ChangesAnnotation], err)
	}

	again, err := svc.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Workloads) != 0 || len(again.Unchanged) != 1 {
		t.Errorf("re-apply: workloads %+v, unchanged %v; want no-op", again.Workloads, names(again.Unchanged))
	}
}

func TestApplyKeepsExplicitValuesThroughThePatch(t *testing.T) {
	// Same tricky case as TestExplicitValuesArePreserved, but through the real strategic
	// merge patch path: the patch must only add fields, never replace the container list.
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "partial", nil,
		corev1.Container{
			Name: "web", Image: "nginx",
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](101)},
		},
		corev1.Container{Name: "sidecar", Image: "busybox"},
	))
	if _, err := svc.Apply(context.Background(), Request{Namespaces: []string{"legacy"}}); err != nil {
		t.Fatal(err)
	}
	cs := getDeployment(t, client, "legacy", "partial").Spec.Template.Spec.Containers
	if len(cs) != 2 || cs[1].Image != "busybox" {
		t.Fatalf("containers = %+v", cs)
	}
	if got := cs[0].Resources.Requests.Cpu().String(); got != "100m" {
		t.Errorf("explicit CPU request = %s, want 100m", got)
	}
	if ptr.Deref(cs[0].SecurityContext.RunAsUser, -1) != 101 {
		t.Error("explicit runAsUser lost")
	}
	if cs[1].SecurityContext == nil || cs[1].Resources.Limits.Memory().IsZero() {
		t.Error("second container not hardened")
	}
}

func TestApplyRetriesOnConflict(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	conflicts := 0
	client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "unhardened",
				errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	plan, err := svc.Apply(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if conflicts != 1 || plan.Workloads[0].Result != ResultPatched {
		t.Errorf("conflicts=%d result=%s, want one conflict then patched", conflicts, plan.Workloads[0].Result)
	}
}

func TestApplyCarriesOnPastFailures(t *testing.T) {
	svc, client := newTestService(namespace("legacy"),
		deployment("legacy", "broken", nil), deployment("legacy", "fine", nil))
	client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchActionImpl).GetName() == "broken" {
			return true, nil, errors.New("admission webhook denied the request")
		}
		return false, nil, nil
	})
	plan, err := svc.Apply(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]WorkloadPlan{}
	for _, w := range plan.Workloads {
		results[w.Name] = w
	}
	if results["broken"].Result != ResultFailed || !strings.Contains(results["broken"].Error, "webhook") {
		t.Errorf("broken = %+v", results["broken"])
	}
	if results["fine"].Result != ResultPatched {
		t.Errorf("fine = %+v; one failure must not stop the rest", results["fine"])
	}
}

func TestPlanScope(t *testing.T) {
	bare := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "legacy", Name: "debug"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	owned := bare.DeepCopy()
	owned.Name = "unhardened-abc"
	owned.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "unhardened-abc", UID: "1"}}

	svc, _ := newTestService(namespace("legacy"),
		deployment("legacy", "unhardened", nil),
		deployment("legacy", "db", map[string]string{guard.IgnoreLabel: "true"}),
		hardenedDeployment("legacy", "gateway"),
		bare, owned,
	)
	plan, err := svc.Plan(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Workloads) != 1 || plan.Workloads[0].Name != "unhardened" {
		t.Errorf("workloads = %+v, want only unhardened", plan.Workloads)
	}
	if got := names(plan.Unchanged); len(got) != 1 || got[0] != "gateway" {
		t.Errorf("unchanged = %v, want [gateway]", got)
	}
	skipped := map[string]string{}
	for _, s := range plan.Skipped {
		skipped[s.Kind+"/"+s.Name] = s.Reason
	}
	if !strings.Contains(skipped["Deployment/db"], guard.IgnoreLabel) {
		t.Errorf("opted-out deployment not skipped: %v", skipped)
	}
	if !strings.Contains(skipped["Pod/debug"], "bare Pod") {
		t.Errorf("bare pod not reported: %v", skipped)
	}
	// Controller-owned pods are covered by their controller, not reported separately.
	if _, ok := skipped["Pod/unhardened-abc"]; ok {
		t.Error("controller-owned pod reported")
	}
}

func TestLimitRangeDefaultsSkipResources(t *testing.T) {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Namespace: "legacy", Name: "defaults"},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:    corev1.LimitTypeContainer,
			Default: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		}}},
	}
	svc, _ := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil), lr)
	plan, err := svc.Plan(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plan.Workloads[0].Changes {
		if strings.HasPrefix(c.Field, "resources.") {
			t.Errorf("set %s although the LimitRange provides defaults", c.Field)
		}
	}
	if len(plan.Notes) != 1 {
		t.Errorf("notes = %v, want one about the LimitRange", plan.Notes)
	}
}

func TestRequestErrors(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), namespace("kube-system"), deployment("legacy", "unhardened", nil))

	tests := []struct {
		name  string
		req   Request
		check func(error) bool
	}{
		{"no namespaces", Request{}, func(err error) bool { var e *InvalidError; return errors.As(err, &e) }},
		{"bad level", Request{Namespaces: []string{"legacy"}, Level: "max"}, func(err error) bool { var e *InvalidError; return errors.As(err, &e) }},
		{"missing namespace", Request{Namespaces: []string{"nope"}}, func(err error) bool { var e *NamespaceNotFoundError; return errors.As(err, &e) }},
		{"protected namespace", Request{Namespaces: []string{"kube-system"}}, func(err error) bool { var e *guard.ProtectedError; return errors.As(err, &e) }},
		// The protected namespace is listed second: nothing in legacy may be patched first.
		{"protected after a valid one", Request{Namespaces: []string{"legacy", "kube-system"}}, func(err error) bool { var e *guard.ProtectedError; return errors.As(err, &e) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.Apply(context.Background(), tt.req); !tt.check(err) {
				t.Errorf("unexpected error %v", err)
			}
		})
	}
	if getDeployment(t, client, "legacy", "unhardened").Annotations[HardenedAtAnnotation] != "" {
		t.Error("a rejected request patched a workload")
	}
}

func TestRolloutStatus(t *testing.T) {
	dep := func(gen, observed int64, replicas, updated, available, total int32, paused bool) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Paused: paused},
			Status: appsv1.DeploymentStatus{ObservedGeneration: observed, UpdatedReplicas: updated,
				AvailableReplicas: available, Replicas: total},
		}
	}
	tests := []struct {
		name        string
		obj         object
		done, final bool
	}{
		{"deployment rolled out", dep(2, 2, 2, 2, 2, 2, false), true, true},
		{"controller hasn't seen the change", dep(2, 1, 2, 2, 2, 2, false), false, false},
		// Old pods still count as available; the rollout isn't done until they're replaced.
		{"old pods still running", dep(2, 2, 2, 2, 2, 3, false), false, false},
		{"new pods not available", dep(2, 2, 2, 2, 1, 2, false), false, false},
		{"paused", dep(2, 2, 2, 0, 2, 2, true), false, true},
		{"statefulset OnDelete", &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}}}, false, true},
		{"daemonset rolled out", &appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3}}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, final, msg := rolloutStatus(tt.obj)
			if done != tt.done || final != tt.final {
				t.Errorf("done=%v final=%v (%s), want %v/%v", done, final, msg, tt.done, tt.final)
			}
		})
	}
}

func TestContainerProblem(t *testing.T) {
	tests := []struct {
		name string
		st   corev1.ContainerStatus
		want string
	}{
		{"healthy", corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, ""},
		{"starting", corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}, ""},
		{"crash loop", corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}, "CrashLoopBackOff"},
		// What runAsNonRoot does to an image that runs as root.
		{"refused by kubelet", corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}}}, "CreateContainerConfigError"},
		{"just exited", corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}, "Error (exit 1)"},
		// Running again right now, but it crashed since the rollout: no readiness probe hides this.
		{"restarted", corev1.ContainerStatus{RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, "restarted 2 times"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerProblem(tt.st); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChangesAnnotationAccumulates(t *testing.T) {
	// Baseline, then strict: the record must keep both rounds, or an undo would forget the
	// first round's fields.
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	svc.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	first, err := svc.Apply(ctx, Request{Namespaces: []string{"legacy"}, Level: Baseline})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Apply(ctx, Request{Namespaces: []string{"legacy"}, Level: Strict})
	if err != nil {
		t.Fatal(err)
	}
	d := getDeployment(t, client, "legacy", "unhardened")
	var recorded []Change
	if err := json.Unmarshal([]byte(d.Annotations[ChangesAnnotation]), &recorded); err != nil {
		t.Fatal(err)
	}
	if want := len(first.Workloads[0].Changes) + len(second.Workloads[0].Changes); len(recorded) != want {
		t.Errorf("recorded %d changes, want %d", len(recorded), want)
	}
	if d.Annotations[LevelAnnotation] != "strict" || d.Annotations[HardenedAtAnnotation] != "2026-10-05T12:00:00Z" {
		t.Errorf("annotations = %v", d.Annotations)
	}
}
