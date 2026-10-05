package hardening

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func mustApply(t *testing.T, svc *Service, level Level) {
	t.Helper()
	if _, err := svc.Apply(context.Background(), Request{Namespaces: []string{"legacy"}, Level: level}); err != nil {
		t.Fatal(err)
	}
}

func partialSpec() *corev1.PodSpec {
	return &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init", Image: "busybox"}},
		Containers: []corev1.Container{{
			Name: "web", Image: "nginx",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
			}},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:    ptr.To[int64](101),
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}},
			},
		}},
	}
}

// The bonus requirement: harden, then undo, and the spec is exactly what it was. Baseline
// and then strict, so the change record holds two rounds; the original has explicit values
// that were never the tool's to remove.
func TestUndoRestoresOriginalSpec(t *testing.T) {
	dep := deployment("legacy", "partial", nil)
	dep.Spec.Template.Spec = *partialSpec()
	original := dep.Spec.Template.Spec.DeepCopy()
	svc, client := newTestService(namespace("legacy"), dep)

	mustApply(t, svc, Baseline)
	mustApply(t, svc, Strict)
	if reflect.DeepEqual(&getDeployment(t, client, "legacy", "partial").Spec.Template.Spec, original) {
		t.Fatal("hardening changed nothing; the test proves nothing")
	}

	plan, err := svc.UndoApply(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Workloads) != 1 || plan.Workloads[0].Result != ResultPatched || len(plan.Workloads[0].Notes) != 0 {
		t.Fatalf("undo = %+v", plan.Workloads)
	}
	got := getDeployment(t, client, "legacy", "partial")
	if !reflect.DeepEqual(&got.Spec.Template.Spec, original) {
		t.Errorf("spec after undo differs from the original:\n got  %+v\n want %+v", got.Spec.Template.Spec, *original)
	}
	for _, a := range []string{ChangesAnnotation, LevelAnnotation, HardenedAtAnnotation} {
		if _, ok := got.Annotations[a]; ok {
			t.Errorf("annotation %s left behind", a)
		}
	}
}

// Undo must not revert someone's later edit: a field that no longer holds the tool's value
// belongs to whoever changed it.
func TestUndoLeavesDriftedFields(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	mustApply(t, svc, Baseline)

	// The owner raises the memory limit and turns privilege escalation back on.
	d := getDeployment(t, client, "legacy", "unhardened")
	c := &d.Spec.Template.Spec.Containers[0]
	c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
	c.SecurityContext.AllowPrivilegeEscalation = ptr.To(true)
	if _, err := client.AppsV1().Deployments("legacy").Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	plan, err := svc.UndoApply(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if notes := plan.Workloads[0].Notes; len(notes) != 2 {
		t.Errorf("notes = %v, want one per drifted field", notes)
	}
	got := getDeployment(t, client, "legacy", "unhardened").Spec.Template.Spec
	gc := got.Containers[0]
	if gc.Resources.Limits.Memory().String() != "512Mi" || !ptr.Deref(gc.SecurityContext.AllowPrivilegeEscalation, false) {
		t.Errorf("drifted fields were reverted: %+v / %+v", gc.Resources, gc.SecurityContext)
	}
	// Everything still holding the tool's value is gone.
	if got.SecurityContext != nil || gc.Resources.Requests != nil {
		t.Errorf("undrifted fields left: pod %+v, requests %v", got.SecurityContext, gc.Resources.Requests)
	}
}

func TestUndoPlanChangesNothingAndUndoIsIdempotent(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	mustApply(t, svc, Baseline)
	hardened := getDeployment(t, client, "legacy", "unhardened")

	plan, err := svc.UndoPlan(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Undo || !plan.DryRun || len(plan.Workloads) != 1 || len(plan.Workloads[0].Changes) != 5 {
		t.Fatalf("undo plan = %+v", plan)
	}
	for _, c := range plan.Workloads[0].Changes {
		if c.Before == nil || c.After != nil {
			t.Errorf("undo change %+v: want before = tool's value, after = nil", c)
		}
	}
	if getDeployment(t, client, "legacy", "unhardened").ResourceVersion != hardened.ResourceVersion {
		t.Error("undo plan modified the deployment")
	}

	if _, err := svc.UndoApply(context.Background(), Request{Namespaces: []string{"legacy"}}); err != nil {
		t.Fatal(err)
	}
	again, err := svc.UndoApply(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Workloads) != 0 {
		t.Errorf("second undo = %+v, want nothing to do", again.Workloads)
	}
}

func TestUndoWithEverythingDriftedOnlyDropsTheRecord(t *testing.T) {
	svc, client := newTestService(namespace("legacy"), deployment("legacy", "unhardened", nil))
	mustApply(t, svc, Baseline)
	d := getDeployment(t, client, "legacy", "unhardened")
	// Someone rewrote the whole template since.
	d.Spec.Template.Spec.SecurityContext = nil
	d.Spec.Template.Spec.Containers[0] = corev1.Container{Name: "web", Image: "nginx:2"}
	if _, err := client.AppsV1().Deployments("legacy").Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	plan, err := svc.UndoApply(context.Background(), Request{Namespaces: []string{"legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	wp := plan.Workloads[0]
	if wp.RestartsPods {
		t.Error("an annotation-only change claims to restart pods")
	}
	if !strings.Contains(strings.Join(wp.Notes, " "), "nothing left to revert") {
		t.Errorf("notes = %v", wp.Notes)
	}
	if _, ok := getDeployment(t, client, "legacy", "unhardened").Annotations[ChangesAnnotation]; ok {
		t.Error("change record left behind")
	}
}

func TestSameValue(t *testing.T) {
	tests := []struct {
		cur, recorded any
		want          bool
	}{
		{true, true, true},
		{false, true, false},
		{"RuntimeDefault", "RuntimeDefault", true},
		// Read back from JSON, []string becomes []any.
		{[]corev1.Capability{"ALL"}, []any{"ALL"}, true},
		{[]corev1.Capability{"ALL", "NET_RAW"}, []any{"ALL"}, false},
		// Quantities compare by value, not spelling.
		{resource.MustParse("500m"), "0.5", true},
		{resource.MustParse("256Mi"), "256Mi", true},
		{resource.MustParse("512Mi"), "256Mi", false},
	}
	for _, tt := range tests {
		if got := sameValue(tt.cur, tt.recorded); got != tt.want {
			t.Errorf("sameValue(%v, %v) = %v, want %v", tt.cur, tt.recorded, got, tt.want)
		}
	}
}
