package api

import (
	"context"
	"net/http"

	"github.com/abbyck/workloadguard/internal/hardening"
)

type planFunc func(context.Context, hardening.Request) (*hardening.Plan, error)

// planHandler serves a dry run: hardening plan or undo plan.
func (s *Server) planHandler(name string, plan planFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req hardening.Request
		if err := decode(r, &req); err != nil {
			writeRequestError(w, err)
			return
		}
		p, err := plan(r.Context(), req)
		if err != nil {
			loggerFrom(r.Context(), s.log).Error(name+" plan failed", "namespaces", req.Namespaces, "err", err)
			writeRequestError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

// applyHandler serves a change to the cluster: hardening apply or undo apply.
func (s *Server) applyHandler(name string, apply planFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.apply(w, r, name, apply) }
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request, name string, apply planFunc) {
	log := loggerFrom(r.Context(), s.log)
	var req hardening.Request
	if err := decode(r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	// Detached from the request, like isolation, and long enough to wait for rollouts.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.hardening.RolloutTimeout()+mutationTimeout)
	defer cancel()
	plan, err := apply(ctx, req)
	if err != nil {
		log.Error(name+" apply failed", "namespaces", req.Namespaces, "err", err)
		writeRequestError(w, err)
		return
	}
	for _, wp := range plan.Workloads {
		log.Info(name, "workload", wp.WorkloadRef.String(), "result", wp.Result,
			"changes", len(wp.Changes), "rollout", wp.Rollout, "error", wp.Error)
	}
	writeJSON(w, http.StatusOK, plan)
}
