package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/abbyck/workloadguard/internal/guard"
)

// fieldManager identifies workloadguard's changes in managedFields.
const fieldManager = "workloadguard"

// rollbackTimeout bounds cleanup after a failed apply, independently of the request's
// deadline, which may be what just expired.
const rollbackTimeout = 10 * time.Second

// Isolation is an active isolation as stored in the cluster.
type Isolation struct {
	ID        string      `json:"id"`
	A         Side        `json:"a"`
	B         Side        `json:"b"`
	CreatedAt time.Time   `json:"createdAt"`
	Policies  []ObjectRef `json:"policies"`
	Warnings  []string    `json:"warnings"`
}

// ObjectRef names an object the isolation owns.
type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Service turns isolations on and off.
type Service struct {
	client   kubernetes.Interface
	guard    *guard.Guard
	podCIDRs []netip.Prefix
}

func NewService(client kubernetes.Interface, g *guard.Guard, podCIDRs []netip.Prefix) *Service {
	return &Service{client: client, guard: g, podCIDRs: podCIDRs}
}

// On turns isolation on. It is idempotent: re-sending a request re-applies the same two
// policies. created is false if both policies already existed.
//
// If the second policy fails, the first is deleted again if this call created it, so a
// failed request never leaves a one-sided isolation behind that nobody asked for.
func (s *Service) On(ctx context.Context, r Request) (iso *Isolation, created bool, err error) {
	warnings, err := Check(ctx, s.client, s.guard, r)
	if err != nil {
		return nil, false, err
	}
	cidrWarnings, err := checkPodCIDRs(ctx, s.client, s.podCIDRs)
	if err != nil {
		return nil, false, err
	}
	warnings = append(warnings, cidrWarnings...)

	policies := Build(r, s.podCIDRs)
	var applied, newlyCreated []*networkingv1.NetworkPolicy
	for _, p := range policies {
		existed, err := s.exists(ctx, p)
		if err != nil {
			return nil, false, errors.Join(err, s.rollback(ctx, newlyCreated))
		}
		out, err := s.apply(ctx, p)
		if err != nil {
			err = fmt.Errorf("apply NetworkPolicy %s/%s: %w", p.Namespace, p.Name, err)
			return nil, false, errors.Join(err, s.rollback(ctx, newlyCreated))
		}
		applied = append(applied, out)
		if !existed {
			newlyCreated = append(newlyCreated, out)
		}
	}

	iso, err = fromPolicies(applied)
	if err != nil {
		return nil, false, err
	}
	iso.Warnings = append(iso.Warnings, warnings...)
	return iso, len(newlyCreated) > 0, nil
}

func (s *Service) exists(ctx context.Context, p *networkingv1.NetworkPolicy) (bool, error) {
	_, err := s.client.NetworkingV1().NetworkPolicies(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("get NetworkPolicy %s/%s: %w", p.Namespace, p.Name, err)
	}
}

// apply creates or updates the policy with server-side apply, so re-applying is a no-op and
// workloadguard owns exactly the fields it sets.
func (s *Service) apply(ctx context.Context, p *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return s.client.NetworkingV1().NetworkPolicies(p.Namespace).Patch(ctx, p.Name, types.ApplyPatchType, data,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr.To(true)})
}

// rollback deletes policies this call created. It uses its own deadline because the
// request's context may be the thing that just expired.
func (s *Service) rollback(ctx context.Context, created []*networkingv1.NetworkPolicy) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	var errs []error
	for _, p := range created {
		err := s.client.NetworkingV1().NetworkPolicies(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("rollback: delete NetworkPolicy %s/%s: %w (delete it by hand)", p.Namespace, p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// fromPolicies rebuilds an Isolation from its policies' labels and request annotation.
func fromPolicies(policies []*networkingv1.NetworkPolicy) (*Isolation, error) {
	if len(policies) == 0 {
		return nil, errors.New("no policies")
	}
	var r Request
	if err := json.Unmarshal([]byte(policies[0].Annotations[RequestAnnotation]), &r); err != nil {
		return nil, fmt.Errorf("NetworkPolicy %s/%s: bad %s annotation: %w",
			policies[0].Namespace, policies[0].Name, RequestAnnotation, err)
	}
	iso := &Isolation{ID: policies[0].Labels[IDLabel], A: r.A, B: r.B, Warnings: []string{}}
	for _, p := range policies {
		iso.Policies = append(iso.Policies, ObjectRef{Namespace: p.Namespace, Name: p.Name})
		if t := p.CreationTimestamp.Time; iso.CreatedAt.IsZero() || (!t.IsZero() && t.Before(iso.CreatedAt)) {
			iso.CreatedAt = t
		}
	}
	return iso, nil
}
