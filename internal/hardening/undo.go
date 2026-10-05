package hardening

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// UndoPlan returns what UndoApply would revert, without changing anything.
func (s *Service) UndoPlan(ctx context.Context, r Request) (*Plan, error) {
	return s.runUndo(ctx, r, true)
}

// UndoApply reverts hardening on the workloads in the requested namespaces. For each field
// the tool recorded, it removes the field only if it still holds the value the tool set.
// A field someone has changed since is theirs now: it's left alone and reported.
func (s *Service) UndoApply(ctx context.Context, r Request) (*Plan, error) {
	return s.runUndo(ctx, r, false)
}

func (s *Service) runUndo(ctx context.Context, r Request, dryRun bool) (*Plan, error) {
	r.Level = "" // not used by undo
	r, err := r.normalize()
	if err != nil {
		return nil, err
	}
	for _, ns := range r.Namespaces {
		if err := s.checkNamespace(ctx, ns); err != nil {
			return nil, err
		}
	}

	plan := &Plan{Undo: true, DryRun: dryRun,
		Workloads: []WorkloadPlan{}, Skipped: []Skipped{}, Unchanged: []WorkloadRef{}, Notes: []string{}}
	for _, ns := range r.Namespaces {
		for _, k := range patchable {
			items, err := k.list(ctx, s.client, ns)
			if err != nil {
				return nil, fmt.Errorf("list %ss in %s: %w", k.name, ns, err)
			}
			for _, o := range items {
				ref := WorkloadRef{Kind: k.name, Namespace: o.GetNamespace(), Name: o.GetName()}
				if _, ok := o.GetAnnotations()[ChangesAnnotation]; !ok {
					continue // never hardened by the tool: nothing to undo, nothing to report
				}
				if err := s.guard.CheckWorkload(k.name, o); err != nil {
					plan.Skipped = append(plan.Skipped, Skipped{WorkloadRef: ref, Reason: err.Error()})
					continue
				}
				build := func(cur object) ([]Change, []string, []byte, error) { return buildUndoPatch(k, cur) }
				if wp := s.workload(ctx, k, o, build, dryRun); wp != nil {
					plan.Workloads = append(plan.Workloads, *wp)
				} else {
					plan.Unchanged = append(plan.Unchanged, ref)
				}
			}
		}
	}
	if !dryRun {
		s.waitForRollouts(ctx, plan)
	}
	return plan, nil
}

// buildUndoPatch reverts the recorded changes on a copy of o and returns them, with notes
// about fields that drifted, and the patch. Changes are reported as before = the value the
// tool set, after = nil (removed).
func buildUndoPatch(k kind, o object) ([]Change, []string, []byte, error) {
	raw, ok := o.GetAnnotations()[ChangesAnnotation]
	if !ok {
		return nil, nil, nil, nil
	}
	var recorded []Change
	if err := json.Unmarshal([]byte(raw), &recorded); err != nil {
		return nil, nil, nil, fmt.Errorf("%s annotation is not valid JSON; fix or remove it by hand: %w", ChangesAnnotation, err)
	}

	modified := o.DeepCopyObject().(object)
	spec := &template(modified).Spec
	var changes []Change
	var notes []string
	for _, c := range recorded {
		reverted, note := revert(spec, c)
		if reverted {
			changes = append(changes, Change{Container: c.Container, Field: c.Field, Before: c.After, After: nil})
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	prune(spec)

	ann := modified.GetAnnotations()
	delete(ann, HardenedAtAnnotation)
	delete(ann, LevelAnnotation)
	delete(ann, ChangesAnnotation)
	modified.SetAnnotations(ann)

	// Nothing left to revert (every field drifted): still drop the record, so the workload
	// stops showing up as hardened. That touches only metadata and restarts nothing.
	patch, err := diffPatch(k, o, modified)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(changes) == 0 {
		notes = append(notes, "nothing left to revert; only the hardening annotations are removed")
		changes = []Change{{Field: "metadata.annotations", Before: ChangesAnnotation, After: nil}}
	}
	return changes, notes, patch, nil
}

// revert removes c's field from spec if it still holds the value the tool set.
func revert(spec *corev1.PodSpec, c Change) (bool, string) {
	f, ok := undoFields[c.Field]
	if !ok {
		return false, fmt.Sprintf("unknown field %q in the change record; left as is", c.Field)
	}
	var ctr *corev1.Container
	if c.Container != "" {
		if ctr = findContainer(spec, c.Container); ctr == nil {
			return false, fmt.Sprintf("container %s no longer exists; %s not reverted", c.Container, c.Field)
		}
	}
	cur, present := f.get(spec, ctr)
	if !present {
		return false, "" // already gone
	}
	if !sameValue(cur, c.After) {
		where := c.Field
		if c.Container != "" {
			where = c.Container + " " + c.Field
		}
		return false, fmt.Sprintf("%s changed since hardening (now %v, the tool set %v); left as is", where, cur, c.After)
	}
	f.clear(spec, ctr)
	return true, ""
}

// sameValue compares a live value with one read back from the JSON change record, where
// []string has become []any. Quantities are compared by value, so "0.5" equals "500m".
func sameValue(cur, recorded any) bool {
	if q, ok := cur.(resource.Quantity); ok {
		s, ok := recorded.(string)
		if !ok {
			return false
		}
		r, err := resource.ParseQuantity(s)
		return err == nil && q.Cmp(r) == 0
	}
	a, errA := json.Marshal(cur)
	b, errB := json.Marshal(recorded)
	return errA == nil && errB == nil && string(a) == string(b)
}

func findContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return &spec.InitContainers[i]
		}
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

type fieldOps struct {
	get   func(spec *corev1.PodSpec, c *corev1.Container) (any, bool)
	clear func(spec *corev1.PodSpec, c *corev1.Container)
}

// undoFields knows how to read and remove every field hardenPodSpec sets.
var undoFields = map[string]fieldOps{
	"securityContext.seccompProfile.type": {
		get: func(s *corev1.PodSpec, _ *corev1.Container) (any, bool) {
			if s.SecurityContext == nil || s.SecurityContext.SeccompProfile == nil {
				return nil, false
			}
			return string(s.SecurityContext.SeccompProfile.Type), true
		},
		clear: func(s *corev1.PodSpec, _ *corev1.Container) { s.SecurityContext.SeccompProfile = nil },
	},
	"securityContext.runAsNonRoot": {
		get: func(s *corev1.PodSpec, _ *corev1.Container) (any, bool) {
			if s.SecurityContext == nil || s.SecurityContext.RunAsNonRoot == nil {
				return nil, false
			}
			return *s.SecurityContext.RunAsNonRoot, true
		},
		clear: func(s *corev1.PodSpec, _ *corev1.Container) { s.SecurityContext.RunAsNonRoot = nil },
	},
	"securityContext.allowPrivilegeEscalation": {
		get: func(_ *corev1.PodSpec, c *corev1.Container) (any, bool) {
			if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil {
				return nil, false
			}
			return *c.SecurityContext.AllowPrivilegeEscalation, true
		},
		clear: func(_ *corev1.PodSpec, c *corev1.Container) { c.SecurityContext.AllowPrivilegeEscalation = nil },
	},
	"securityContext.capabilities.drop": {
		get: func(_ *corev1.PodSpec, c *corev1.Container) (any, bool) {
			if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || c.SecurityContext.Capabilities.Drop == nil {
				return nil, false
			}
			return c.SecurityContext.Capabilities.Drop, true
		},
		clear: func(_ *corev1.PodSpec, c *corev1.Container) { c.SecurityContext.Capabilities.Drop = nil },
	},
	"securityContext.readOnlyRootFilesystem": {
		get: func(_ *corev1.PodSpec, c *corev1.Container) (any, bool) {
			if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil {
				return nil, false
			}
			return *c.SecurityContext.ReadOnlyRootFilesystem, true
		},
		clear: func(_ *corev1.PodSpec, c *corev1.Container) { c.SecurityContext.ReadOnlyRootFilesystem = nil },
	},
	"resources.requests.cpu":    resourceField(func(c *corev1.Container) corev1.ResourceList { return c.Resources.Requests }, corev1.ResourceCPU),
	"resources.requests.memory": resourceField(func(c *corev1.Container) corev1.ResourceList { return c.Resources.Requests }, corev1.ResourceMemory),
	"resources.limits.memory":   resourceField(func(c *corev1.Container) corev1.ResourceList { return c.Resources.Limits }, corev1.ResourceMemory),
}

func resourceField(list func(*corev1.Container) corev1.ResourceList, name corev1.ResourceName) fieldOps {
	return fieldOps{
		get: func(_ *corev1.PodSpec, c *corev1.Container) (any, bool) {
			q, ok := list(c)[name]
			return q, ok
		},
		clear: func(_ *corev1.PodSpec, c *corev1.Container) { delete(list(c), name) },
	}
}

// prune drops structs and maps that reverting left empty, so the spec matches what it was
// before hardening instead of gaining "securityContext: {}" or "requests: {}".
func prune(spec *corev1.PodSpec) {
	if sc := spec.SecurityContext; sc != nil && reflect.DeepEqual(*sc, corev1.PodSecurityContext{}) {
		spec.SecurityContext = nil
	}
	for _, list := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range list {
			c := &list[i]
			if sc := c.SecurityContext; sc != nil {
				if caps := sc.Capabilities; caps != nil && len(caps.Add) == 0 && len(caps.Drop) == 0 {
					sc.Capabilities = nil
				}
				if reflect.DeepEqual(*sc, corev1.SecurityContext{}) {
					c.SecurityContext = nil
				}
			}
			if len(c.Resources.Requests) == 0 {
				c.Resources.Requests = nil
			}
			if len(c.Resources.Limits) == 0 {
				c.Resources.Limits = nil
			}
		}
	}
}
