package hardening

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// hardenPodSpec fills in missing fields of spec in place and returns what it set, plus notes
// on what it deliberately left alone. Its one rule: only set fields that are unset, and
// never loosen or override an explicit value. skipResources leaves resources alone (the
// namespace's LimitRange already provides defaults).
func hardenPodSpec(spec *corev1.PodSpec, level Level, d Defaults, skipResources bool) ([]Change, []string) {
	h := &hardener{spec: spec, level: level, defaults: d}
	h.pod()
	for i := range spec.InitContainers {
		h.container(&spec.InitContainers[i], skipResources)
	}
	for i := range spec.Containers {
		h.container(&spec.Containers[i], skipResources)
	}
	return h.changes, h.notes
}

type hardener struct {
	spec     *corev1.PodSpec
	level    Level
	defaults Defaults
	changes  []Change
	notes    []string
}

func (h *hardener) set(container, field string, after any) {
	h.changes = append(h.changes, Change{Container: container, Field: field, Before: nil, After: after})
}

func (h *hardener) note(format string, args ...any) {
	h.notes = append(h.notes, fmt.Sprintf(format, args...))
}

// podSC returns the pod securityContext, creating it only when a field is about to be set,
// so an untouched spec doesn't gain an empty securityContext: {}.
func (h *hardener) podSC() *corev1.PodSecurityContext {
	if h.spec.SecurityContext == nil {
		h.spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	return h.spec.SecurityContext
}

func (h *hardener) pod() {
	if h.spec.SecurityContext == nil || h.spec.SecurityContext.SeccompProfile == nil {
		h.podSC().SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
		h.set("", "securityContext.seccompProfile.type", string(corev1.SeccompProfileTypeRuntimeDefault))
	}

	if h.level != Strict {
		return
	}
	if h.spec.SecurityContext != nil && h.spec.SecurityContext.RunAsNonRoot != nil {
		return
	}
	// runAsNonRoot with an explicit runAsUser: 0 makes the kubelet refuse to start the
	// container. Someone asked for root on purpose; report it instead of breaking the pod.
	if root := h.explicitRootUser(); root != "" {
		h.note("runAsNonRoot not set: %s explicitly runs as root (runAsUser: 0)", root)
		return
	}
	h.podSC().RunAsNonRoot = ptr.To(true)
	h.set("", "securityContext.runAsNonRoot", true)
	h.note("strict: runAsNonRoot fails at startup if the image's user is root or not numeric")
}

func (h *hardener) explicitRootUser() string {
	if sc := h.spec.SecurityContext; sc != nil && ptr.Deref(sc.RunAsUser, -1) == 0 {
		return "the pod"
	}
	for _, c := range slices.Concat(h.spec.InitContainers, h.spec.Containers) {
		if c.SecurityContext != nil && ptr.Deref(c.SecurityContext.RunAsUser, -1) == 0 {
			return "container " + c.Name
		}
	}
	return ""
}

func (h *hardener) container(c *corev1.Container, skipResources bool) {
	h.securityContext(c)
	if !skipResources {
		h.resources(c)
	}
}

func (h *hardener) securityContext(c *corev1.Container) {
	sc := c.SecurityContext
	if sc != nil && ptr.Deref(sc.Privileged, false) {
		// Privileged containers usually need it (CNI agents, storage drivers), and the API
		// rejects allowPrivilegeEscalation=false alongside privileged=true anyway.
		h.note("container %s is privileged: securityContext left unchanged; review it by hand", c.Name)
		return
	}
	csc := func() *corev1.SecurityContext {
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		return c.SecurityContext
	}

	if sc == nil || sc.AllowPrivilegeEscalation == nil {
		if addsCapability(c, "SYS_ADMIN") {
			// The API rejects allowPrivilegeEscalation=false together with CAP_SYS_ADMIN.
			h.note("container %s adds SYS_ADMIN: allowPrivilegeEscalation left unset", c.Name)
		} else {
			csc().AllowPrivilegeEscalation = ptr.To(false)
			h.set(c.Name, "securityContext.allowPrivilegeEscalation", false)
		}
	}

	if h.level != Strict {
		return
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) == 0 {
		if csc().Capabilities == nil {
			c.SecurityContext.Capabilities = &corev1.Capabilities{}
		}
		// Explicitly added capabilities stay: drop ALL is applied first, then add.
		c.SecurityContext.Capabilities.Drop = []corev1.Capability{"ALL"}
		h.set(c.Name, "securityContext.capabilities.drop", []string{"ALL"})
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		csc().ReadOnlyRootFilesystem = ptr.To(true)
		h.set(c.Name, "securityContext.readOnlyRootFilesystem", true)
		h.note("strict: container %s gets a read-only root filesystem; paths it writes to need an emptyDir", c.Name)
	}
}

func addsCapability(c *corev1.Container, name corev1.Capability) bool {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return false
	}
	return slices.Contains(c.SecurityContext.Capabilities.Add, name) ||
		slices.Contains(c.SecurityContext.Capabilities.Add, "CAP_"+name)
}

// resources fills missing requests and a missing memory limit. It never sets a CPU limit:
// CPU limits cause throttling even when the node has spare CPU, while a memory limit
// protects the node from one container eating all its memory.
//
// When only a limit is set, the API server defaults the request to the limit at admission,
// so the request isn't missing in practice and is left alone. Setting it here would lower
// it and change the pod's QoS class.
func (h *hardener) resources(c *corev1.Container) {
	req, lim := c.Resources.Requests, c.Resources.Limits
	setReq := func(name corev1.ResourceName, q resource.Quantity) {
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[name] = q
		h.set(c.Name, "resources.requests."+string(name), q.String())
	}

	if _, ok := req[corev1.ResourceCPU]; !ok {
		if _, hasLimit := lim[corev1.ResourceCPU]; !hasLimit {
			setReq(corev1.ResourceCPU, h.defaults.CPURequest)
		}
	}
	if _, ok := req[corev1.ResourceMemory]; !ok {
		if _, hasLimit := lim[corev1.ResourceMemory]; !hasLimit {
			setReq(corev1.ResourceMemory, h.defaults.MemoryRequest)
		}
	}
	if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
		limit := h.defaults.MemoryLimit
		// A limit below the request is rejected by the API server.
		if r, ok := c.Resources.Requests[corev1.ResourceMemory]; ok && r.Cmp(limit) > 0 {
			limit = r
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits[corev1.ResourceMemory] = limit
		h.set(c.Name, "resources.limits.memory", limit.String())
	}
}
