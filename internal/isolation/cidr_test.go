package isolation

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func node(name string, podCIDRs ...string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{PodCIDRs: podCIDRs}}
}

func TestParsePodCIDRs(t *testing.T) {
	got, err := ParsePodCIDRs([]string{"10.244.1.7/16"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].String() != "10.244.0.0/16" {
		t.Errorf("got %s, want host bits masked to 10.244.0.0/16", got[0])
	}
	if _, err := ParsePodCIDRs([]string{"10.244.0.0"}); err == nil {
		t.Error("want error for a missing prefix length")
	}
	if _, err := ParsePodCIDRs(nil); err == nil {
		t.Error("want error for an empty list")
	}
}

func TestCheckPodCIDRs(t *testing.T) {
	configured, _ := ParsePodCIDRs([]string{"10.244.0.0/16"})

	tests := []struct {
		name         string
		nodes        []runtime.Object
		wantErr      bool
		wantWarnings int
	}{
		{"kind: per-node /24s inside the /16", []runtime.Object{node("cp", "10.244.0.0/24"), node("w1", "10.244.1.0/24")}, false, 0},
		// A node outside the configured range means its pods would match the ipBlock.
		{"node outside the range", []runtime.Object{node("cp", "10.244.0.0/24"), node("w9", "10.99.0.0/24")}, true, 0},
		{"node range wider than configured", []runtime.Object{node("cp", "10.244.0.0/15")}, true, 0},
		{"dual-stack node with uncovered IPv6", []runtime.Object{node("cp", "10.244.0.0/24", "fd00::/64")}, true, 0},
		{"CNI with its own IPAM: can't verify", []runtime.Object{node("cp")}, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := checkPodCIDRs(context.Background(), fake.NewClientset(tt.nodes...), configured)
			var cidrErr *PodCIDRError
			if tt.wantErr != errors.As(err, &cidrErr) {
				t.Fatalf("err = %v, want PodCIDRError: %v", err, tt.wantErr)
			}
			if len(warnings) != tt.wantWarnings {
				t.Errorf("warnings = %v, want %d", warnings, tt.wantWarnings)
			}
		})
	}
}
