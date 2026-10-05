package isolation

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ParsePodCIDRs parses CIDR strings such as "10.244.0.0/16".
func ParsePodCIDRs(cidrs []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("pod CIDR %q: %w", c, err)
		}
		out = append(out, p.Masked())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one pod CIDR is required")
	}
	return out, nil
}

// PodCIDRError means the configured pod CIDRs don't cover the cluster's pod IPs, so the
// ipBlock rule would let the isolated peer back in. It's a server misconfiguration.
type PodCIDRError struct {
	Uncovered []string // "node: cidr"
}

func (e *PodCIDRError) Error() string {
	return "configured --pod-cidrs don't cover these node pod ranges, so isolation would leak: " +
		strings.Join(e.Uncovered, ", ")
}

// checkPodCIDRs verifies that every node's pod range lies inside a configured pod CIDR.
// It runs on every isolation request, so a node added later with a new range is caught too.
// Nodes without spec.podCIDRs (CNIs that do their own IPAM) can't be verified; that is
// returned as a warning rather than blocking.
func checkPodCIDRs(ctx context.Context, client kubernetes.Interface, configured []netip.Prefix) ([]string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes to check pod CIDRs: %w", err)
	}
	var uncovered, unknown []string
	for _, n := range nodes.Items {
		if len(n.Spec.PodCIDRs) == 0 {
			unknown = append(unknown, n.Name)
			continue
		}
		for _, c := range n.Spec.PodCIDRs {
			p, err := netip.ParsePrefix(c)
			if err != nil || !covered(p, configured) {
				uncovered = append(uncovered, n.Name+": "+c)
			}
		}
	}
	if len(uncovered) > 0 {
		return nil, &PodCIDRError{Uncovered: uncovered}
	}
	if len(unknown) > 0 {
		return []string{"can't verify --pod-cidrs: nodes without spec.podCIDRs: " + strings.Join(unknown, ", ")}, nil
	}
	return nil, nil
}

func covered(p netip.Prefix, configured []netip.Prefix) bool {
	for _, c := range configured {
		if c.Bits() <= p.Bits() && c.Contains(p.Addr()) {
			return true
		}
	}
	return false
}
