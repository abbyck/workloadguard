package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abbyck/workloadguard/internal/guard"
)

func newTestServer(ready ReadyFunc) *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), ready, nil, nil)
}

func ok(context.Context) error { return nil }

// decodeError reads the standard error body and returns its code.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	return body.Error.Code
}

func TestProbes(t *testing.T) {
	down := func(context.Context) error { return errors.New("connection refused") }

	tests := []struct {
		name       string
		ready      ReadyFunc
		path       string
		wantStatus int
	}{
		{"healthz ok", ok, "/healthz", http.StatusOK},
		// The pod must not be restarted just because the API server is down.
		{"healthz ignores API outage", down, "/healthz", http.StatusOK},
		{"readyz ok", ok, "/readyz", http.StatusOK},
		{"readyz fails when API unreachable", down, "/readyz", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestServer(tt.ready).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestReadyzRespectsTimeout(t *testing.T) {
	// A hung API server must fail the probe, not hang it.
	hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	rec := httptest.NewRecorder()
	newTestServer(hang).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestUnknownRouteIsJSON404(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(ok).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec); code != "not_found" {
		t.Errorf("code = %q, want not_found", code)
	}
}

func TestRequestID(t *testing.T) {
	h := newTestServer(ok).Handler()

	t.Run("generated when missing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if got := rec.Header().Get(requestIDHeader); len(got) != 16 {
			t.Errorf("request ID = %q, want 16 hex chars", got)
		}
	})

	t.Run("caller's ID echoed back", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(requestIDHeader, "abc-123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get(requestIDHeader); got != "abc-123" {
			t.Errorf("request ID = %q, want abc-123", got)
		}
	})
}

func TestPanicBecomes500(t *testing.T) {
	s := newTestServer(ok)
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec); code != "internal" {
		t.Errorf("code = %q, want internal", code)
	}
}

func TestDecode(t *testing.T) {
	type side struct {
		Namespace string            `json:"namespace"`
		Selector  map[string]string `json:"selector"`
	}

	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int // 0 means decode succeeds
	}{
		{"json", "application/json", `{"namespace":"tenant-a","selector":{"app":"gateway"}}`, 0},
		{"yaml", "application/yaml", "namespace: tenant-a\nselector:\n  app: gateway\n", 0},
		{"content type with charset", "application/json; charset=utf-8", `{"namespace":"tenant-a"}`, 0},
		// A typo must not silently become an empty selector, which would match every pod.
		{"unknown field rejected", "application/yaml", "namespace: tenant-a\nselecter:\n  app: gateway\n", http.StatusBadRequest},
		{"malformed", "application/json", `{"namespace":`, http.StatusBadRequest},
		{"empty", "application/json", "", http.StatusBadRequest},
		{"unsupported content type", "text/plain", "namespace: tenant-a", http.StatusUnsupportedMediaType},
		{"too large", "application/yaml", "namespace: " + strings.Repeat("a", maxBodyBytes), http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got side
			var decodeErr error
			// Go through limitBody so the size limit is exercised the way the server applies it.
			h := limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if decodeErr = decode(r, &got); decodeErr != nil {
					writeRequestError(w, decodeErr)
				}
			}))
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if tt.wantStatus == 0 {
				if decodeErr != nil {
					t.Fatalf("decode: %v", decodeErr)
				}
				if got.Namespace != "tenant-a" {
					t.Errorf("namespace = %q, want tenant-a", got.Namespace)
				}
				return
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (err: %v)", rec.Code, tt.wantStatus, decodeErr)
			}
		})
	}
}

func TestProtectedErrorIs403(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRequestError(rec, &guard.ProtectedError{Kind: "Namespace", Name: "kube-system", Reason: "in the protected namespace list"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if code := decodeError(t, rec); code != "protected" {
		t.Errorf("code = %q, want protected", code)
	}
}
