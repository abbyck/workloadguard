package hardening

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// object is a typed workload: Deployment, StatefulSet or DaemonSet.
type object interface {
	runtime.Object
	metav1.Object
}

// kind holds the typed client calls for one patchable workload kind.
type kind struct {
	name string
	// schema is an empty object of the kind, used to compute a strategic merge patch
	// (which merges lists like containers by name instead of replacing them).
	schema object
	list   func(ctx context.Context, c kubernetes.Interface, ns string) ([]object, error)
	get    func(ctx context.Context, c kubernetes.Interface, ns, name string) (object, error)
	patch  func(ctx context.Context, c kubernetes.Interface, ns, name string, data []byte, opts metav1.PatchOptions) (object, error)
}

// patchable are the kinds whose pod template is patched. Their controllers roll the change
// out to new pods.
var patchable = []kind{
	{
		name:   "Deployment",
		schema: &appsv1.Deployment{},
		list: func(ctx context.Context, c kubernetes.Interface, ns string) ([]object, error) {
			l, err := c.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			return objects(l.Items), nil
		},
		get: func(ctx context.Context, c kubernetes.Interface, ns, name string) (object, error) {
			return c.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		},
		patch: func(ctx context.Context, c kubernetes.Interface, ns, name string, data []byte, opts metav1.PatchOptions) (object, error) {
			return c.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, data, opts)
		},
	},
	{
		name:   "StatefulSet",
		schema: &appsv1.StatefulSet{},
		list: func(ctx context.Context, c kubernetes.Interface, ns string) ([]object, error) {
			l, err := c.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			return objects(l.Items), nil
		},
		get: func(ctx context.Context, c kubernetes.Interface, ns, name string) (object, error) {
			return c.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		},
		patch: func(ctx context.Context, c kubernetes.Interface, ns, name string, data []byte, opts metav1.PatchOptions) (object, error) {
			return c.AppsV1().StatefulSets(ns).Patch(ctx, name, types.StrategicMergePatchType, data, opts)
		},
	},
	{
		name:   "DaemonSet",
		schema: &appsv1.DaemonSet{},
		list: func(ctx context.Context, c kubernetes.Interface, ns string) ([]object, error) {
			l, err := c.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			return objects(l.Items), nil
		},
		get: func(ctx context.Context, c kubernetes.Interface, ns, name string) (object, error) {
			return c.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		},
		patch: func(ctx context.Context, c kubernetes.Interface, ns, name string, data []byte, opts metav1.PatchOptions) (object, error) {
			return c.AppsV1().DaemonSets(ns).Patch(ctx, name, types.StrategicMergePatchType, data, opts)
		},
	},
}

// objects converts a list's items to []object.
func objects[T any, PT interface {
	*T
	object
}](items []T) []object {
	out := make([]object, len(items))
	for i := range items {
		out[i] = PT(&items[i])
	}
	return out
}

func template(o object) *corev1.PodTemplateSpec {
	switch w := o.(type) {
	case *appsv1.Deployment:
		return &w.Spec.Template
	case *appsv1.StatefulSet:
		return &w.Spec.Template
	case *appsv1.DaemonSet:
		return &w.Spec.Template
	}
	panic(fmt.Sprintf("unsupported workload type %T", o))
}

func selector(o object) *metav1.LabelSelector {
	switch w := o.(type) {
	case *appsv1.Deployment:
		return w.Spec.Selector
	case *appsv1.StatefulSet:
		return w.Spec.Selector
	case *appsv1.DaemonSet:
		return w.Spec.Selector
	}
	panic(fmt.Sprintf("unsupported workload type %T", o))
}

// rolloutStatus reports whether the workload's latest pod template is fully rolled out.
// final means waiting longer won't change the answer (paused, OnDelete).
func rolloutStatus(o object) (done, final bool, msg string) {
	switch w := o.(type) {
	case *appsv1.Deployment:
		if w.Spec.Paused {
			return false, true, "paused: new pods start when the Deployment is resumed"
		}
		if w.Status.ObservedGeneration < w.Generation {
			return false, false, "waiting for the controller to see the change"
		}
		want := replicas(w.Spec.Replicas)
		if w.Status.UpdatedReplicas == want && w.Status.AvailableReplicas == want && w.Status.Replicas == want {
			return true, true, "ready"
		}
		return false, false, fmt.Sprintf("%d of %d pods updated, %d available (old pods count until replaced)",
			w.Status.UpdatedReplicas, want, w.Status.AvailableReplicas)
	case *appsv1.StatefulSet:
		if w.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			return false, true, "OnDelete update strategy: pods pick up the change when deleted"
		}
		if w.Status.ObservedGeneration < w.Generation {
			return false, false, "waiting for the controller to see the change"
		}
		want := replicas(w.Spec.Replicas)
		// A partitioned rolling update only replaces pods with an ordinal at or above the
		// partition; the rest keep the old template until the partition is lowered.
		if p := partition(w); p > 0 {
			wantUpdated := max(want-p, 0)
			if w.Status.UpdatedReplicas >= wantUpdated && w.Status.ReadyReplicas == want {
				return true, true, fmt.Sprintf("ready; partition %d keeps pods below ordinal %d on the old template", p, p)
			}
			return false, false, fmt.Sprintf("%d of %d pods above the partition updated, %d ready", w.Status.UpdatedReplicas, wantUpdated, w.Status.ReadyReplicas)
		}
		if w.Status.UpdatedReplicas == want && w.Status.ReadyReplicas == want && w.Status.CurrentRevision == w.Status.UpdateRevision {
			return true, true, "ready"
		}
		return false, false, fmt.Sprintf("%d of %d pods updated, %d ready", w.Status.UpdatedReplicas, want, w.Status.ReadyReplicas)
	case *appsv1.DaemonSet:
		if w.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
			return false, true, "OnDelete update strategy: pods pick up the change when deleted"
		}
		if w.Status.ObservedGeneration < w.Generation {
			return false, false, "waiting for the controller to see the change"
		}
		want := w.Status.DesiredNumberScheduled
		if w.Status.UpdatedNumberScheduled == want && w.Status.NumberAvailable == want {
			return true, true, "ready"
		}
		return false, false, fmt.Sprintf("%d of %d pods updated, %d available", w.Status.UpdatedNumberScheduled, want, w.Status.NumberAvailable)
	}
	panic(fmt.Sprintf("unsupported workload type %T", o))
}

func partition(s *appsv1.StatefulSet) int32 {
	if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		return *ru.Partition
	}
	return 0
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}
