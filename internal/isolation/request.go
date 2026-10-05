// Package isolation blocks traffic between two workloads with NetworkPolicies.
package isolation

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/abbyck/workloadguard/internal/guard"
)

// Side is one of the two workloads: the pods in Namespace matching every label in Selector.
type Side struct {
	Namespace string            `json:"namespace"`
	Selector  map[string]string `json:"selector"`
}

func (s Side) String() string {
	return s.Namespace + "/" + labels.SelectorFromSet(s.Selector).String()
}

// Request asks for traffic between A and B to be blocked in both directions.
type Request struct {
	A Side `json:"a"`
	B Side `json:"b"`
}

// InvalidError lists everything wrong with a request, so the operator can fix it in one go.
type InvalidError struct {
	Problems []string
}

func (e *InvalidError) Error() string {
	return "invalid isolation request: " + strings.Join(e.Problems, "; ")
}

// NamespaceNotFoundError means a side names a namespace that doesn't exist.
type NamespaceNotFoundError struct {
	Namespace string
}

func (e *NamespaceNotFoundError) Error() string {
	return fmt.Sprintf("namespace %q not found", e.Namespace)
}

// Validate checks the request on its own, without the cluster. It returns *InvalidError.
func (r Request) Validate() error {
	var problems []string
	problems = append(problems, r.A.validate("a")...)
	problems = append(problems, r.B.validate("b")...)
	if len(problems) == 0 && r.A.Namespace == r.B.Namespace && canOverlap(r.A.Selector, r.B.Selector) {
		// Equal selectors are the obvious case. Selectors with no conflicting key are the
		// subtle one: {app: gateway} and {tier: edge} both match a pod labeled with both,
		// and a pod can't be isolated from itself. Rejecting on what *could* match, not what
		// matches today, keeps the request valid as pods come and go.
		problems = append(problems, fmt.Sprintf(
			"a and b are in the same namespace and their selectors can match the same pods (%s vs %s); "+
				"give them a label key with different values", r.A, r.B))
	}
	if len(problems) > 0 {
		return &InvalidError{Problems: problems}
	}
	return nil
}

func (s Side) validate(name string) []string {
	var problems []string
	if s.Namespace == "" {
		problems = append(problems, name+".namespace is required")
	} else {
		for _, msg := range validation.IsDNS1123Label(s.Namespace) {
			problems = append(problems, fmt.Sprintf("%s.namespace %q: %s", name, s.Namespace, msg))
		}
	}
	// An empty selector matches every pod in the namespace. That is almost certainly a
	// mistake, and isolating a whole namespace deserves a more deliberate request.
	if len(s.Selector) == 0 {
		problems = append(problems, name+".selector must have at least one label")
	}
	for _, k := range slices.Sorted(maps.Keys(s.Selector)) {
		for _, msg := range validation.IsQualifiedName(k) {
			problems = append(problems, fmt.Sprintf("%s.selector key %q: %s", name, k, msg))
		}
		for _, msg := range validation.IsValidLabelValue(s.Selector[k]) {
			problems = append(problems, fmt.Sprintf("%s.selector[%s] value %q: %s", name, k, s.Selector[k], msg))
		}
	}
	return problems
}

// canOverlap reports whether some pod could match both label selectors: true unless they
// require different values for the same key.
func canOverlap(a, b map[string]string) bool {
	for k, va := range a {
		if vb, ok := b[k]; ok && va != vb {
			return false
		}
	}
	return true
}

// Check validates the request against the cluster: both namespaces exist and aren't
// protected, and no currently matching pod has opted out. It returns warnings for things
// that are allowed but suspicious. Errors are *InvalidError, *NamespaceNotFoundError,
// *guard.ProtectedError, or an API error.
func Check(ctx context.Context, client kubernetes.Interface, g *guard.Guard, r Request) ([]string, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	var warnings []string
	for _, side := range []struct {
		name string
		Side
	}{{"a", r.A}, {"b", r.B}} {
		w, err := checkSide(ctx, client, g, side.name, side.Side)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, w...)
	}
	return warnings, nil
}

func checkSide(ctx context.Context, client kubernetes.Interface, g *guard.Guard, name string, s Side) ([]string, error) {
	ns, err := client.CoreV1().Namespaces().Get(ctx, s.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &NamespaceNotFoundError{Namespace: s.Namespace}
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace %s: %w", s.Namespace, err)
	}
	if err := g.CheckNamespace(ns); err != nil {
		return nil, err
	}

	pods, err := client.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(s.Selector).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods for %s: %w", name, err)
	}
	for i := range pods.Items {
		if err := g.CheckWorkload("Pod", &pods.Items[i]); err != nil {
			return nil, err
		}
	}
	if len(pods.Items) == 0 {
		// Allowed: the policy still applies to pods created later. But it's likely a typo.
		return []string{fmt.Sprintf("%s selector %s matches no pods right now", name, s)}, nil
	}
	return nil, nil
}
