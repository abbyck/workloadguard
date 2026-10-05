package api

import (
	"context"
	"net/http"

	"github.com/abbyck/workloadguard/internal/hardening"
)

func (s *Server) planHardening(w http.ResponseWriter, r *http.Request) {
	var req hardening.Request
	if err := decode(r, &req); err != nil {
		writeRequestError(w, err)
		return
	}
	plan, err := s.hardening.Plan(r.Context(), req)
	if err != nil {
		loggerFrom(r.Context(), s.log).Error("hardening plan failed", "namespaces", req.Namespaces, "err", err)
		writeRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) applyHardening(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)
	var req hardening.Request
	if err := decode(r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	// Detached from the request, like isolation, and long enough to wait for rollouts.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.hardening.RolloutTimeout()+mutationTimeout)
	defer cancel()
	plan, err := s.hardening.Apply(ctx, req)
	if err != nil {
		log.Error("hardening apply failed", "namespaces", req.Namespaces, "err", err)
		writeRequestError(w, err)
		return
	}
	for _, wp := range plan.Workloads {
		log.Info("hardening", "workload", wp.WorkloadRef.String(), "result", wp.Result,
			"changes", len(wp.Changes), "rollout", wp.Rollout, "error", wp.Error)
	}
	writeJSON(w, http.StatusOK, plan)
}
