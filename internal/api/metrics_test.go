package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/abbyck/workloadguard/internal/metrics"
)

func TestRequestsAreCountedByRoutePattern(t *testing.T) {
	h := newFlowServer()
	before := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("/v1/isolations/{id}", "GET", "404"))

	// Two different IDs must land in the same series, labeled by pattern, not path.
	do(t, h, "GET", "/v1/isolations/iso-0000000001", "")
	do(t, h, "GET", "/v1/isolations/iso-0000000002", "")

	after := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("/v1/isolations/{id}", "GET", "404"))
	if after-before != 2 {
		t.Errorf("counted %v requests under the route pattern, want 2", after-before)
	}
	unmatched := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("unmatched", "GET", "404"))
	do(t, h, "GET", "/nope", "")
	if testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("unmatched", "GET", "404"))-unmatched != 1 {
		t.Error("unknown paths should be counted as route=unmatched")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newFlowServer()
	do(t, h, "POST", "/v1/hardening/apply", "namespaces: [tenant-a]\n")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"workloadguard_http_requests_total",
		"workloadguard_http_request_duration_seconds",
		"go_goroutines",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics is missing %s", name)
		}
	}
}
