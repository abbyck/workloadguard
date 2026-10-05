package isolation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/netip"
	"slices"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels and annotations on every object workloadguard creates. The labels are how the tool
// finds its own objects again; nothing is kept in memory.
const (
	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ManagedByValue    = "workloadguard"
	IDLabel           = "workloadguard.io/isolation-id"
	SideLabel         = "workloadguard.io/side"
	RequestAnnotation = "workloadguard.io/request"

	// Set by the API server on every namespace since Kubernetes 1.21.
	namespaceNameLabel = "kubernetes.io/metadata.name"
)

// canonical orders the sides so that {a: X, b: Y} and {a: Y, b: X} are the same isolation.
func (r Request) canonical() Request {
	if r.B.String() < r.A.String() {
		r.A, r.B = r.B, r.A
	}
	return r
}

// ID derives the isolation's ID from the request itself, so sending the same request twice
// finds the same objects instead of creating new ones.
func (r Request) ID() string {
	b, _ := json.Marshal(r.canonical()) // can't fail: only strings and string maps
	sum := sha256.Sum256(b)
	return "iso-" + hex.EncodeToString(sum[:5])
}

// Build returns the two NetworkPolicies that isolate A and B: one selecting A's pods that
// allows all traffic except to and from B, and the mirror image for B. Either policy alone
// blocks A<->B; having both means a single deleted or broken policy doesn't reopen it.
//
// podCIDRs must cover every pod IP in the cluster; see allExcept.
func Build(r Request, podCIDRs []netip.Prefix) []*networkingv1.NetworkPolicy {
	r = r.canonical()
	id := r.ID()
	req, _ := json.Marshal(r)
	return []*networkingv1.NetworkPolicy{
		policyFor(id, "a", r.A, r.B, podCIDRs, string(req)),
		policyFor(id, "b", r.B, r.A, podCIDRs, string(req)),
	}
}

func policyFor(id, side string, self, peer Side, podCIDRs []netip.Prefix, req string) *networkingv1.NetworkPolicy {
	peers := allExcept(peer, podCIDRs)
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      PolicyName(id, side),
			Namespace: self.Namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
				IDLabel:        id,
				SideLabel:      side,
			},
			Annotations: map[string]string{RequestAnnotation: req},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: maps.Clone(self.Selector)},
			// Selecting the pods for both types makes them deny-by-default in both directions;
			// the rules below then allow everything except the peer.
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: peers}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers}},
		},
	}
}

// PolicyName is the name of side's policy. The side suffix keeps the two names apart when
// both workloads are in the same namespace.
func PolicyName(id, side string) string {
	return "workloadguard-" + id + "-" + side
}

// allExcept returns peers that together match every traffic source/destination except the
// pods selected by peer. NetworkPolicy can only allow, so "not B" is spelled out as a union:
//
//  1. pods in every namespace other than B's
//  2. pods in B's namespace that don't match B's selector. B's selector is an AND of labels,
//     so its negation is an OR: NOT(k1=v1 AND k2=v2) = k1!=v1 OR k2!=v2. That needs one peer
//     per label, since the requirements inside a single selector are ANDed. NotIn also
//     matches pods that don't have the key at all, which is what we want.
//  3. everything that isn't a pod: external addresses, nodes (kubelet probes) and
//     host-network pods. Most CNIs match ipBlock against pod IPs too, so without excepting
//     the pod CIDRs this rule would let B straight back in.
func allExcept(peer Side, podCIDRs []netip.Prefix) []networkingv1.NetworkPolicyPeer {
	peers := []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: namespaceNameLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{peer.Namespace},
		}}},
	}}

	for _, k := range slices.Sorted(maps.Keys(peer.Selector)) {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: peer.Namespace}},
			PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: k, Operator: metav1.LabelSelectorOpNotIn, Values: []string{peer.Selector[k]},
			}}},
		})
	}

	var v4, v6 []string
	for _, p := range podCIDRs {
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	peers = append(peers,
		networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: v4}},
		networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: v6}},
	)
	return peers
}
