package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two contexts pointing at different servers, so the test can tell which one was picked.
const testKubeconfig = `apiVersion: v1
kind: Config
current-context: first
clusters:
- name: first
  cluster: {server: "https://first.example:6443"}
- name: second
  cluster: {server: "https://second.example:6443"}
users:
- name: dev
  user: {token: "not-a-real-token"}
contexts:
- name: first
  context: {cluster: first, user: dev}
- name: second
  context: {cluster: second, user: dev}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRESTConfig(t *testing.T) {
	path := writeKubeconfig(t)

	tests := []struct {
		name       string
		opts       Options
		wantServer string
		wantErr    string
	}{
		{
			name:       "explicit kubeconfig uses its current context",
			opts:       Options{Kubeconfig: path},
			wantServer: "https://first.example:6443",
		},
		{
			name:       "context flag overrides current context",
			opts:       Options{Kubeconfig: path, Context: "second"},
			wantServer: "https://second.example:6443",
		},
		{
			name:    "unknown context is an error, not a silent fallback",
			opts:    Options{Kubeconfig: path, Context: "missing"},
			wantErr: "missing",
		},
		{
			name:    "missing kubeconfig file is an error",
			opts:    Options{Kubeconfig: filepath.Join(t.TempDir(), "nope")},
			wantErr: "kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := RESTConfig(tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Host != tt.wantServer {
				t.Errorf("host = %q, want %q", cfg.Host, tt.wantServer)
			}
			if cfg.QPS != qps || cfg.Burst != burst || cfg.Timeout != requestTimeout {
				t.Errorf("limits = %v/%v/%v, want %v/%v/%v", cfg.QPS, cfg.Burst, cfg.Timeout, qps, burst, requestTimeout)
			}
			if cfg.UserAgent != userAgent {
				t.Errorf("user agent = %q, want %q", cfg.UserAgent, userAgent)
			}
		})
	}
}
