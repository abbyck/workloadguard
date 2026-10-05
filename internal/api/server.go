// Package api serves workloadguard's HTTP API.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/abbyck/workloadguard/internal/guard"
	"github.com/abbyck/workloadguard/internal/isolation"
)

// ReadyFunc reports whether the service can do its job, i.e. reach the Kubernetes API.
type ReadyFunc func(ctx context.Context) error

// readyTimeout bounds the readiness check, so a hung API server makes the probe fail
// instead of making it wait for the kubelet's own timeout.
const readyTimeout = 2 * time.Second

// Server holds the dependencies shared by all handlers.
type Server struct {
	log       *slog.Logger
	ready     ReadyFunc
	guard     *guard.Guard
	isolation *isolation.Service
}

func New(log *slog.Logger, ready ReadyFunc, g *guard.Guard, iso *isolation.Service) *Server {
	return &Server{log: log, ready: ready, guard: g, isolation: iso}
}

// Handler returns the API with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("POST /v1/isolations", s.createIsolation)
	mux.HandleFunc("GET /v1/isolations", s.listIsolations)
	mux.HandleFunc("GET /v1/isolations/{id}", s.getIsolation)
	mux.HandleFunc("DELETE /v1/isolations/{id}", s.deleteIsolation)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path)
	})

	var h http.Handler = mux
	h = limitBody(h)
	h = s.recoverPanics(h)
	h = s.logRequests(h)
	h = requestID(h)
	return h
}

// healthz answers as long as the process can serve HTTP. It deliberately doesn't check the
// API server: a Kubernetes outage shouldn't make the kubelet restart this pod.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz fails while the Kubernetes API is unreachable, so the Service stops routing
// requests here that would only fail anyway.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		loggerFrom(r.Context(), s.log).Warn("readiness check failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "kubernetes API unreachable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
