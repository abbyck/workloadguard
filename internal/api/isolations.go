package api

import (
	"context"
	"net/http"
	"time"

	"github.com/abbyck/workloadguard/internal/isolation"
)

// mutationTimeout bounds the Kubernetes calls of a request that changes the cluster.
const mutationTimeout = 30 * time.Second

// isolateBody mirrors the brief's example request: {isolate: {a: ..., b: ...}}.
type isolateBody struct {
	Isolate isolation.Request `json:"isolate"`
}

func (s *Server) createIsolation(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)
	var body isolateBody
	if err := decode(r, &body); err != nil {
		writeRequestError(w, err)
		return
	}

	// Detached from the request: if the client disconnects halfway, the work still finishes
	// (or rolls back) instead of being cancelled between the two policies.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), mutationTimeout)
	defer cancel()
	iso, created, err := s.isolation.On(ctx, body.Isolate)
	if err != nil {
		log.Error("isolation on failed", "a", body.Isolate.A.String(), "b", body.Isolate.B.String(), "err", err)
		writeRequestError(w, err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	log.Info("isolation on", "id", iso.ID, "a", iso.A.String(), "b", iso.B.String(), "created", created)
	writeJSON(w, status, iso)
}
