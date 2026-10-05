// Package kube builds the Kubernetes client used by the rest of workloadguard.
package kube

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Options controls how the client finds and talks to the cluster.
type Options struct {
	// Kubeconfig is an explicit kubeconfig path. Empty means: in-cluster config if running
	// in a pod, otherwise the standard loading rules ($KUBECONFIG, then ~/.kube/config).
	Kubeconfig string
	// Context overrides the kubeconfig's current context. Ignored in-cluster.
	Context string
}

const (
	userAgent = "workloadguard"
	// The client-go defaults (5 QPS, burst 10) are tight for listing workloads across several
	// namespaces in one request. These stay well below what the API server tolerates.
	qps   = 20
	burst = 40
	// Backstop for any request that isn't given a shorter context deadline by its caller.
	requestTimeout = 30 * time.Second
)

// RESTConfig resolves the cluster connection. An explicit kubeconfig or context always wins,
// so a developer can point a locally running binary at a specific cluster even from inside
// a pod; otherwise in-cluster config is tried before the standard kubeconfig locations.
func RESTConfig(opts Options) (*rest.Config, error) {
	cfg, err := resolve(opts)
	if err != nil {
		return nil, err
	}
	cfg.QPS = qps
	cfg.Burst = burst
	cfg.Timeout = requestTimeout
	cfg.UserAgent = userAgent // shows up as-is in API server audit logs
	return cfg, nil
}

func resolve(opts Options) (*rest.Config, error) {
	if opts.Kubeconfig == "" && opts.Context == "" {
		cfg, err := rest.InClusterConfig()
		if err == nil {
			return cfg, nil
		}
		if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = opts.Kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.Context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return cfg, nil
}

// NewClient returns a typed clientset. Callers should depend on kubernetes.Interface,
// not the concrete type, so tests can pass k8s.io/client-go/kubernetes/fake instead.
func NewClient(opts Options) (kubernetes.Interface, error) {
	cfg, err := RESTConfig(opts)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
