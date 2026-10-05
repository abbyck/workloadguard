package hardening

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

var testDefaults = Defaults{
	CPURequest:    resource.MustParse("50m"),
	MemoryRequest: resource.MustParse("64Mi"),
	MemoryLimit:   resource.MustParse("256Mi"),
}

func fields(changes []Change) map[string]any {
	out := map[string]any{}
	for _, c := range changes {
		out[c.Container+":"+c.Field] = c.After
	}
	return out
}

func TestBaselineOnBareSpec(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}
	changes, _ := hardenPodSpec(spec, Baseline, testDefaults, false)

	want := map[string]any{
		":securityContext.seccompProfile.type":         "RuntimeDefault",
		"web:securityContext.allowPrivilegeEscalation": false,
		"web:resources.requests.cpu":                   "50m",
		"web:resources.requests.memory":                "64Mi",
		"web:resources.limits.memory":                  "256Mi",
	}
	if got := fields(changes); !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %v\nwant      %v", got, want)
	}
	// The reported changes must match what was actually written to the spec.
	c := spec.Containers[0]
	if *c.SecurityContext.AllowPrivilegeEscalation || spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Error("spec not updated to match the reported changes")
	}
	// Baseline must not touch fields that can break images (see README: hardening levels).
	if c.SecurityContext.Capabilities != nil || c.SecurityContext.ReadOnlyRootFilesystem != nil || spec.SecurityContext.RunAsNonRoot != nil {
		t.Errorf("baseline set a strict-only field: %+v / %+v", c.SecurityContext, spec.SecurityContext)
	}
	// Never a CPU limit.
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Error("CPU limit was set")
	}
}

func TestStrictAddsStrictFields(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}
	changes, notes := hardenPodSpec(spec, Strict, testDefaults, true)
	got := fields(changes)
	for _, f := range []string{
		":securityContext.runAsNonRoot",
		"web:securityContext.capabilities.drop",
		"web:securityContext.readOnlyRootFilesystem",
	} {
		if _, ok := got[f]; !ok {
			t.Errorf("strict didn't set %s", f)
		}
	}
	if len(notes) == 0 {
		t.Error("strict should warn about what it can break")
	}
}

// The tricky case for hardening: a partially configured container. Every explicit value
// must survive, including ones that look "insecure", and only the gaps are filled.
func TestExplicitValuesArePreserved(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init"}},
		Containers: []corev1.Container{{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                ptr.To[int64](101),
				AllowPrivilegeEscalation: ptr.To(true), // explicit, even if unwise
				Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}},
			},
		}},
	}
	changes, _ := hardenPodSpec(spec, Strict, testDefaults, false)
	got := fields(changes)
	web := spec.Containers[0]

	if web.Resources.Requests.Cpu().String() != "100m" || web.Resources.Requests.Memory().String() != "512Mi" {
		t.Errorf("explicit requests changed: %v", web.Resources.Requests)
	}
	if _, ok := got["web:resources.requests.cpu"]; ok {
		t.Error("reported a change to an explicit request")
	}
	// The default limit (256Mi) is below the 512Mi request; the API would reject it.
	if lim := web.Resources.Limits.Memory().String(); lim != "512Mi" {
		t.Errorf("memory limit = %s, want 512Mi (not below the request)", lim)
	}
	if !*web.SecurityContext.AllowPrivilegeEscalation {
		t.Error("explicit allowPrivilegeEscalation: true was overridden")
	}
	if *web.SecurityContext.RunAsUser != 101 {
		t.Error("explicit runAsUser changed")
	}
	if !reflect.DeepEqual(web.SecurityContext.Capabilities.Add, []corev1.Capability{"NET_BIND_SERVICE"}) {
		t.Errorf("explicit capabilities.add changed: %v", web.SecurityContext.Capabilities.Add)
	}
	if !reflect.DeepEqual(web.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("drop = %v, want [ALL] alongside the kept add", web.SecurityContext.Capabilities.Drop)
	}
	// Init containers are covered too.
	if _, ok := got["init:securityContext.allowPrivilegeEscalation"]; !ok {
		t.Error("init container not hardened")
	}
}

func TestLimitOnlyLeavesRequestToAdmission(t *testing.T) {
	// With only a limit, the API server sets request = limit. Filling in the small default
	// request would lower it and turn a Guaranteed pod into a Burstable one.
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name: "web",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		}},
	}}}
	changes, _ := hardenPodSpec(spec, Baseline, testDefaults, false)
	for f := range fields(changes) {
		if f == "web:resources.requests.cpu" || f == "web:resources.requests.memory" || f == "web:resources.limits.memory" {
			t.Errorf("changed %s although a limit was set", f)
		}
	}
}

func TestExplicitRootIsNotOverridden(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name:            "db",
		SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](0)},
	}}}
	changes, notes := hardenPodSpec(spec, Strict, testDefaults, true)
	// runAsNonRoot + runAsUser: 0 means the kubelet refuses to start the container.
	if spec.SecurityContext.RunAsNonRoot != nil {
		t.Error("runAsNonRoot set on a pod that explicitly runs as root")
	}
	if _, ok := fields(changes)[":securityContext.runAsNonRoot"]; ok {
		t.Error("reported runAsNonRoot change")
	}
	if len(notes) == 0 {
		t.Error("want a note explaining why runAsNonRoot was skipped")
	}
}

func TestPrivilegedContainerIsLeftAlone(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name:            "cni",
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
	}}}
	changes, notes := hardenPodSpec(spec, Strict, testDefaults, true)
	for f := range fields(changes) {
		if f != ":securityContext.seccompProfile.type" && f != ":securityContext.runAsNonRoot" {
			t.Errorf("changed %s on a privileged container", f)
		}
	}
	if len(notes) == 0 {
		t.Error("privileged container should be reported")
	}
}

func TestSysAdminKeepsPrivilegeEscalationUnset(t *testing.T) {
	// The API rejects allowPrivilegeEscalation=false together with CAP_SYS_ADMIN.
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name:            "fuse",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}},
	}}}
	hardenPodSpec(spec, Baseline, testDefaults, true)
	if spec.Containers[0].SecurityContext.AllowPrivilegeEscalation != nil {
		t.Error("allowPrivilegeEscalation set alongside SYS_ADMIN")
	}
}

func TestAlreadyHardenedIsNoOp(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}
	hardenPodSpec(spec, Strict, testDefaults, false)
	// Re-applying must change nothing, so a second apply is a no-op and doesn't restart pods.
	changes, _ := hardenPodSpec(spec, Strict, testDefaults, false)
	if len(changes) != 0 {
		t.Errorf("second run changed %v", fields(changes))
	}
}

func TestUntouchedSpecGetsNoEmptyStructs(t *testing.T) {
	spec := &corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: "RuntimeDefault"}},
		Containers: []corev1.Container{{
			Name:            "web",
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
		}},
	}
	before := spec.DeepCopy()
	if changes, _ := hardenPodSpec(spec, Baseline, testDefaults, true); len(changes) != 0 {
		t.Fatalf("changes = %v", fields(changes))
	}
	if !reflect.DeepEqual(spec, before) {
		t.Error("spec changed although nothing was reported")
	}
}
