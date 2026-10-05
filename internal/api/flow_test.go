package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abbyck/workloadguard/internal/guard"
	"github.com/abbyck/workloadguard/internal/hardening"
	"github.com/abbyck/workloadguard/internal/isolation"
)

// These tests go through the real handler stack against a fake cluster, to pin down the
// status codes and response shapes the README documents.

const isolateYAML = `isolate:
  a: {namespace: tenant-a, selector: {app: gateway}}
  b: {namespace: tenant-b, selector: {app: dashboard}}
`

func newFlowServer(objs ...runtime.Object) http.Handler {
	base := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-b"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/24"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "gateway-1", Labels: map[string]string{"app": "gateway"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-b", Name: "dashboard-1", Labels: map[string]string{"app": "dashboard"}}},
	}
	client := fake.NewClientset(append(base, objs...)...)
	// The fake client ignores DryRun and would store a plan's patches. Answer dry runs
	// without storing them, as the real API server does.
	client.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		if len(pa.PatchOptions.DryRun) == 0 {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
		return true, obj, err
	})
	g := guard.New(guard.DefaultProtectedNamespaces, "")
	defaults := hardening.Defaults{
		CPURequest: resource.MustParse("50m"), MemoryRequest: resource.MustParse("64Mi"), MemoryLimit: resource.MustParse("256Mi"),
	}
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), ok,
		isolation.NewService(client, g, []netip.Prefix{netip.MustParsePrefix("10.244.0.0/16")}),
		hardening.NewService(client, g, defaults, 0),
	).Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/yaml")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: response is not JSON: %q", method, path, rec.Body.String())
	}
	return rec.Code, out
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestIsolationLifecycleOverHTTP(t *testing.T) {
	h := newFlowServer()

	code, body := do(t, h, "POST", "/v1/isolations", isolateYAML)
	if code != http.StatusCreated {
		t.Fatalf("on: %d %v", code, body)
	}
	id, _ := body["id"].(string)

	if code, _ := do(t, h, "POST", "/v1/isolations", isolateYAML); code != http.StatusOK {
		t.Errorf("re-send: %d, want 200", code)
	}
	if code, body := do(t, h, "GET", "/v1/isolations", ""); code != http.StatusOK || len(body["isolations"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, body)
	}
	if code, _ := do(t, h, "GET", "/v1/isolations/"+id, ""); code != http.StatusOK {
		t.Errorf("get: %d", code)
	}
	if code, body := do(t, h, "DELETE", "/v1/isolations/"+id, ""); code != http.StatusOK || len(body["deleted"].([]any)) != 2 {
		t.Errorf("off: %d %v", code, body)
	}
	if code, body := do(t, h, "DELETE", "/v1/isolations/"+id, ""); code != http.StatusOK || len(body["deleted"].([]any)) != 0 {
		t.Errorf("off again: %d %v, want 200 with nothing deleted", code, body)
	}
	if code, body := do(t, h, "GET", "/v1/isolations/"+id, ""); code != http.StatusNotFound || errorCode(body) != "isolation_not_found" {
		t.Errorf("get after off: %d %v", code, body)
	}
}

func TestIsolationErrorsOverHTTP(t *testing.T) {
	existing := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-b", Name: "dashboard-only"},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "dashboard"}}},
	}
	tests := []struct {
		name     string
		server   http.Handler
		body     string
		wantCode int
		wantErr  string
	}{
		{"empty selector", newFlowServer(), "isolate:\n  a: {namespace: tenant-a, selector: {}}\n  b: {namespace: tenant-b, selector: {app: dashboard}}\n", 400, "invalid_request"},
		{"typo in a field", newFlowServer(), "isolate:\n  a: {namespace: tenant-a, selecter: {app: gateway}}\n", 400, "bad_body"},
		{"missing namespace", newFlowServer(), strings.Replace(isolateYAML, "tenant-b", "nope", 1), 400, "namespace_not_found"},
		{"protected namespace", newFlowServer(), strings.Replace(isolateYAML, "tenant-b", "kube-system", 1), 403, "protected"},
		{"existing policy", newFlowServer(existing), isolateYAML, 409, "existing_policies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := do(t, tt.server, "POST", "/v1/isolations", tt.body)
			if code != tt.wantCode || errorCode(body) != tt.wantErr {
				t.Errorf("got %d %q, want %d %q (%v)", code, errorCode(body), tt.wantCode, tt.wantErr, body)
			}
		})
	}
	// Whole namespaces: accepted when asked for explicitly; a misspelt field is rejected
	// instead of leaving an empty selector behind.
	wholeNS := "isolate:\n  a: {namespace: tenant-a, allPods: true}\n  b: {namespace: tenant-b, allPods: true}\n"
	if code, body := do(t, newFlowServer(), "POST", "/v1/isolations", wholeNS); code != 201 {
		t.Errorf("whole namespaces: %d %v, want 201", code, body)
	}
	if code, body := do(t, newFlowServer(), "POST", "/v1/isolations", strings.Replace(wholeNS, "allPods", "allpods", 1)); code != 400 || errorCode(body) != "bad_body" {
		t.Errorf("misspelt allPods: %d %v, want 400 bad_body", code, body)
	}
	if code, body := do(t, newFlowServer(), "DELETE", "/v1/isolations/x,app=gateway", ""); code != 400 || errorCode(body) != "invalid_request" {
		t.Errorf("malformed ID: %d %v", code, body)
	}
}

func TestHardeningOverHTTP(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "gateway"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx"}}}},
		},
	}
	h := newFlowServer(dep)

	code, body := do(t, h, "POST", "/v1/hardening/plan", "namespaces: [tenant-a]\n")
	if code != http.StatusOK || body["dryRun"] != true || len(body["workloads"].([]any)) != 1 {
		t.Fatalf("plan: %d %v", code, body)
	}
	code, body = do(t, h, "POST", "/v1/hardening/apply", "namespaces: [tenant-a]\n")
	if code != http.StatusOK {
		t.Fatalf("apply: %d %v", code, body)
	}
	if w := body["workloads"].([]any)[0].(map[string]any); w["result"] != "patched" {
		t.Errorf("apply result = %v", w)
	}
	for _, tt := range []struct {
		body     string
		wantCode int
		wantErr  string
	}{
		{"namespaces: [kube-system]\n", 403, "protected"},
		{"namespaces: [nope]\n", 400, "namespace_not_found"},
		{"namespaces: [tenant-a]\nlevel: max\n", 400, "invalid_request"},
	} {
		if code, body := do(t, h, "POST", "/v1/hardening/plan", tt.body); code != tt.wantCode || errorCode(body) != tt.wantErr {
			t.Errorf("%q: got %d %q, want %d %q", tt.body, code, errorCode(body), tt.wantCode, tt.wantErr)
		}
	}
}
