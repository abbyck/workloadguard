package isolation

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// ConflictError means NetworkPolicies already select the target pods. NetworkPolicies are
// ORed together, so adding "allow everything except the peer" on top of them would allow
// traffic they currently block, and turning isolation off couldn't tell what to restore.
type ConflictError struct {
	Policies []ConflictingPolicy
}

// ConflictingPolicy is an existing policy that selects one side's pods.
type ConflictingPolicy struct {
	ObjectRef
	// IsolationID is set when the policy belongs to another workloadguard isolation.
	IsolationID string `json:"isolationID,omitempty"`
}

func (e *ConflictError) Error() string {
	names := make([]string, 0, len(e.Policies))
	for _, p := range e.Policies {
		n := p.Namespace + "/" + p.Name
		if p.IsolationID != "" {
			n += " (workloadguard isolation " + p.IsolationID + ")"
		}
		names = append(names, n)
	}
	return "NetworkPolicies already select the target pods: " + strings.Join(names, ", ") +
		". NetworkPolicies are additive, so isolating on top of them would allow traffic they block today; " +
		"remove or adjust them first"
}

// conflicts returns the existing NetworkPolicies that select either side's pods, apart from
// this isolation's own policies (a re-apply). Other workloadguard isolations count: two
// "everything except X" policies on the same pods add up to "everything", so stacking
// isolations would silently undo both.
//
// A policy counts as selecting a side if it matches any pod the side selects right now, or
// a pod carrying exactly the side's labels, so it's caught even while no such pod runs.
func conflicts(ctx context.Context, client kubernetes.Interface, r Request) ([]ConflictingPolicy, error) {
	id := r.ID()
	var found []ConflictingPolicy
	seen := map[ObjectRef]bool{}
	for _, s := range []Side{r.A, r.B} {
		policies, err := client.NetworkingV1().NetworkPolicies(s.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list NetworkPolicies in %s: %w", s.Namespace, err)
		}
		pods, err := client.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(s.Selector).String(),
		})
		if err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", s.Namespace, err)
		}
		podLabels := []labels.Set{labels.Set(s.Selector)}
		for _, p := range pods.Items {
			podLabels = append(podLabels, labels.Set(p.Labels))
		}

		for i := range policies.Items {
			np := &policies.Items[i]
			ref := ObjectRef{Namespace: np.Namespace, Name: np.Name}
			if np.Labels[IDLabel] == id || seen[ref] {
				continue
			}
			hit, err := selectsAny(np, podLabels)
			if err != nil {
				return nil, err
			}
			if hit {
				seen[ref] = true
				found = append(found, ConflictingPolicy{ObjectRef: ref, IsolationID: np.Labels[IDLabel]})
			}
		}
	}
	return found, nil
}

func selectsAny(np *networkingv1.NetworkPolicy, podLabels []labels.Set) (bool, error) {
	sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	if err != nil {
		return false, fmt.Errorf("NetworkPolicy %s/%s has an invalid podSelector: %w", np.Namespace, np.Name, err)
	}
	for _, l := range podLabels {
		if sel.Matches(l) {
			return true, nil
		}
	}
	return false, nil
}
