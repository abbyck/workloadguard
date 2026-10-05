package isolation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// idPattern matches IDs produced by Request.ID. Checking it before building a label selector
// keeps a crafted ID (say "x,app=foo") from changing what the selector matches.
var idPattern = regexp.MustCompile(`^iso-[0-9a-f]{10}$`)

// NotFoundError means no policies carry the isolation ID.
type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("isolation %q not found", e.ID)
}

func validID(id string) error {
	if !idPattern.MatchString(id) {
		return &InvalidError{Problems: []string{fmt.Sprintf("isolation ID %q is not of the form iso-<10 hex digits>", id)}}
	}
	return nil
}

// owned lists the policies workloadguard created, across all namespaces, optionally only
// those of one isolation. Finding objects by label is the only state the tool relies on.
func (s *Service) owned(ctx context.Context, id string) ([]networkingv1.NetworkPolicy, error) {
	selector := ManagedByLabel + "=" + ManagedByValue
	if id != "" {
		selector += "," + IDLabel + "=" + id
	}
	list, err := s.client.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list workloadguard NetworkPolicies: %w", err)
	}
	return list.Items, nil
}

// Off deletes the isolation's policies and returns the ones it deleted. Only objects
// labeled with this ID are touched, so unrelated policies, including other isolations,
// survive. Because isolation only ever added policies (and refused to stack on existing
// ones), deleting them restores the previous connectivity.
//
// It is safe to repeat: policies already gone are skipped, and an unknown ID returns an
// empty list. If some deletes fail, it carries on with the rest and returns an error that
// names them; retrying finishes the job.
func (s *Service) Off(ctx context.Context, id string) ([]ObjectRef, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	policies, err := s.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	deleted := []ObjectRef{}
	var errs []error
	for _, p := range policies {
		err := s.client.NetworkingV1().NetworkPolicies(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
			// Don't delete an object that was replaced by a different one with the same name.
			Preconditions: &metav1.Preconditions{UID: &p.UID},
		})
		switch {
		case err == nil:
			deleted = append(deleted, ObjectRef{Namespace: p.Namespace, Name: p.Name})
		case apierrors.IsNotFound(err):
			// Already gone: that's the state we want.
		default:
			errs = append(errs, fmt.Errorf("delete NetworkPolicy %s/%s: %w", p.Namespace, p.Name, err))
		}
	}
	if len(errs) > 0 {
		return deleted, fmt.Errorf("isolation %s partly removed (deleted %v); retry to finish: %w", id, deleted, errors.Join(errs...))
	}
	return deleted, nil
}

// Get returns one active isolation.
func (s *Service) Get(ctx context.Context, id string) (*Isolation, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	policies, err := s.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return nil, &NotFoundError{ID: id}
	}
	return fromPolicies(pointers(policies))
}

// List returns all active isolations, oldest first.
func (s *Service) List(ctx context.Context) ([]*Isolation, error) {
	policies, err := s.owned(ctx, "")
	if err != nil {
		return nil, err
	}
	byID := map[string][]*networkingv1.NetworkPolicy{}
	for _, p := range pointers(policies) {
		id := p.Labels[IDLabel]
		byID[id] = append(byID[id], p)
	}
	out := []*Isolation{}
	for _, group := range byID {
		iso, err := fromPolicies(group)
		if err != nil {
			return nil, err
		}
		out = append(out, iso)
	}
	slices.SortFunc(out, func(a, b *Isolation) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func pointers(items []networkingv1.NetworkPolicy) []*networkingv1.NetworkPolicy {
	out := make([]*networkingv1.NetworkPolicy, len(items))
	for i := range items {
		out[i] = &items[i]
	}
	return out
}
