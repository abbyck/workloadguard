package isolation

import (
	"net/netip"
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var kindPodCIDRs = []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16")}

func TestIDIsDeterministicAndSymmetric(t *testing.T) {
	ab := Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")}
	ba := Request{A: ab.B, B: ab.A}
	if ab.ID() != ba.ID() {
		t.Errorf("swapping a and b changed the ID: %s vs %s", ab.ID(), ba.ID())
	}
	// Built separately: map iteration order differs between the two, the ID must not.
	same := Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")}
	if ab.ID() != same.ID() {
		t.Error("ID is not deterministic")
	}
	other := Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "other")}
	if ab.ID() == other.ID() {
		t.Error("different requests got the same ID")
	}
}

func TestBuildMetadata(t *testing.T) {
	r := Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")}
	policies := Build(r, kindPodCIDRs)
	if len(policies) != 2 {
		t.Fatalf("got %d policies, want 2", len(policies))
	}
	for i, p := range policies {
		self := []Side{r.A, r.B}[i]
		if p.Namespace != self.Namespace {
			t.Errorf("policy %d namespace = %s, want %s", i, p.Namespace, self.Namespace)
		}
		if !reflect.DeepEqual(p.Spec.PodSelector.MatchLabels, self.Selector) {
			t.Errorf("policy %d selects %v, want %v", i, p.Spec.PodSelector.MatchLabels, self.Selector)
		}
		if p.Labels[ManagedByLabel] != ManagedByValue || p.Labels[IDLabel] != r.ID() {
			t.Errorf("policy %d labels = %v", i, p.Labels)
		}
		if p.Annotations[RequestAnnotation] == "" {
			t.Errorf("policy %d has no request annotation", i)
		}
		// Both directions, or the pods stay open for whichever type is missing.
		want := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
		if !reflect.DeepEqual(p.Spec.PolicyTypes, want) {
			t.Errorf("policy %d types = %v, want %v", i, p.Spec.PolicyTypes, want)
		}
	}
}

func TestBuildSameNamespaceNamesDontCollide(t *testing.T) {
	r := Request{A: side("shop", "app", "web"), B: side("shop", "app", "db")}
	policies := Build(r, kindPodCIDRs)
	if policies[0].Name == policies[1].Name {
		t.Errorf("both policies are named %s in the same namespace", policies[0].Name)
	}
}

func TestIPBlockExceptsPodCIDRs(t *testing.T) {
	// Without the except, the ipBlock would match B's pod IPs on most CNIs and let B back
	// in. This was checked on kind: removing the except reopens A<->B.
	cidrs := []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16"), netip.MustParsePrefix("fd00:10:244::/56")}
	peers := Build(Request{A: side("tenant-a", "app", "gateway"), B: side("tenant-b", "app", "dashboard")}, cidrs)[0].
		Spec.Ingress[0].From

	var blocks []networkingv1.IPBlock
	for _, p := range peers {
		if p.IPBlock != nil {
			blocks = append(blocks, *p.IPBlock)
		}
	}
	want := []networkingv1.IPBlock{
		{CIDR: "0.0.0.0/0", Except: []string{"10.244.0.0/16"}},
		{CIDR: "::/0", Except: []string{"fd00:10:244::/56"}},
	}
	if !reflect.DeepEqual(blocks, want) {
		t.Errorf("ipBlocks = %+v, want %+v", blocks, want)
	}
}

// fakePod is a traffic source/destination for evaluating peers.
type fakePod struct {
	namespace string
	labels    map[string]string
}

// allowed reports whether any peer admits the pod, using the same selector semantics as a
// NetworkPolicy implementation: namespaceSelector AND podSelector within one peer, peers ORed.
func allowed(t *testing.T, peers []networkingv1.NetworkPolicyPeer, p fakePod) bool {
	t.Helper()
	nsLabels := labels.Set{namespaceNameLabel: p.namespace}
	for _, peer := range peers {
		if peer.IPBlock != nil {
			continue // pods are in the excepted pod CIDR; see TestIPBlockExceptsPodCIDRs
		}
		if !matches(t, peer.NamespaceSelector, nsLabels) {
			continue
		}
		if peer.PodSelector != nil && !matches(t, peer.PodSelector, labels.Set(p.labels)) {
			continue
		}
		return true
	}
	return false
}

func matches(t *testing.T, sel *metav1.LabelSelector, set labels.Set) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatalf("invalid selector %v: %v", sel, err)
	}
	return s.Matches(set)
}

// The tricky case: NetworkPolicy has no "deny", so "everything except B" is a union of
// peers, and B's multi-label selector has to be negated key by key. Getting the negation
// wrong either leaks traffic from B or cuts off pods that merely share one label with B.
func TestPeersAllowEverythingExceptB(t *testing.T) {
	r := Request{
		A: side("tenant-a", "app", "gateway"),
		B: side("tenant-b", "app", "dashboard", "tier", "frontend"),
	}
	// A's policy: its peers must reject exactly B's pods.
	peers := Build(r, kindPodCIDRs)[0].Spec.Ingress[0].From

	tests := []struct {
		name string
		pod  fakePod
		want bool
	}{
		{"B itself", fakePod{"tenant-b", map[string]string{"app": "dashboard", "tier": "frontend"}}, false},
		{"B with extra labels", fakePod{"tenant-b", map[string]string{"app": "dashboard", "tier": "frontend", "v": "2"}}, false},
		{"same app, other tier in B's namespace", fakePod{"tenant-b", map[string]string{"app": "dashboard", "tier": "backend"}}, true},
		{"same tier, other app in B's namespace", fakePod{"tenant-b", map[string]string{"app": "api", "tier": "frontend"}}, true},
		{"missing one of B's keys", fakePod{"tenant-b", map[string]string{"app": "dashboard"}}, true},
		{"no labels in B's namespace", fakePod{"tenant-b", nil}, true},
		{"B's labels in another namespace", fakePod{"shared", map[string]string{"app": "dashboard", "tier": "frontend"}}, true},
		{"DNS", fakePod{"kube-system", map[string]string{"k8s-app": "kube-dns"}}, true},
		{"A's own replicas", fakePod{"tenant-a", map[string]string{"app": "gateway"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowed(t, peers, tt.pod); got != tt.want {
				t.Errorf("allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPeersSameNamespace(t *testing.T) {
	r := Request{A: side("shop", "app", "web"), B: side("shop", "app", "db")}
	policies := Build(r, kindPodCIDRs)
	// Canonical order may have swapped the sides; find each side's policy by its selector.
	var webPeers []networkingv1.NetworkPolicyPeer
	for _, p := range policies {
		if p.Spec.PodSelector.MatchLabels["app"] == "web" {
			webPeers = p.Spec.Ingress[0].From
		}
	}
	if allowed(t, webPeers, fakePod{"shop", map[string]string{"app": "db"}}) {
		t.Error("web's policy admits db")
	}
	if !allowed(t, webPeers, fakePod{"shop", map[string]string{"app": "cache"}}) {
		t.Error("web's policy blocks an unrelated pod in its own namespace")
	}
	if !allowed(t, webPeers, fakePod{"shop", map[string]string{"app": "web"}}) {
		t.Error("web's policy blocks web's own replicas")
	}
}
