package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"sigs.k8s.io/yaml"

	"github.com/abbyck/workloadguard/internal/guard"
	"github.com/abbyck/workloadguard/internal/isolation"
)

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may be gone; nothing useful to do
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: apiError{Code: code, Message: message}})
}

// requestError is a decode failure with the status it should produce.
type requestError struct {
	status int
	code   string
	msg    string
}

func (e *requestError) Error() string { return e.msg }

// writeRequestError maps err to a status: bad input to 4xx, a protected target to 403,
// anything else (Kubernetes API failures, misconfiguration) to 500.
func writeRequestError(w http.ResponseWriter, err error) {
	var (
		re  *requestError
		pe  *guard.ProtectedError
		inv *isolation.InvalidError
		nf  *isolation.NamespaceNotFoundError
		ce  *isolation.ConflictError
		inf *isolation.NotFoundError
	)
	switch {
	case errors.As(err, &re):
		writeError(w, re.status, re.code, re.msg)
	case errors.As(err, &pe):
		writeError(w, http.StatusForbidden, "protected", pe.Error())
	case errors.As(err, &inv):
		writeError(w, http.StatusBadRequest, "invalid_request", inv.Error())
	case errors.As(err, &nf):
		writeError(w, http.StatusBadRequest, "namespace_not_found", nf.Error())
	case errors.As(err, &inf):
		writeError(w, http.StatusNotFound, "isolation_not_found", inf.Error())
	case errors.As(err, &ce):
		writeError(w, http.StatusConflict, "existing_policies", ce.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// decode reads a JSON or YAML request body into v. Unknown fields are rejected, so a typo
// like "selecter" fails loudly instead of silently becoming an empty selector.
func decode(r *http.Request, v any) error {
	switch ct := r.Header.Get("Content-Type"); mediaType(ct) {
	case "", "application/json", "application/yaml", "application/x-yaml", "text/yaml":
	default:
		return &requestError{http.StatusUnsupportedMediaType, "unsupported_media_type",
			fmt.Sprintf("content type %q not supported; use application/json or application/yaml", ct)}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &requestError{http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("request body exceeds %d bytes", tooBig.Limit)}
		}
		return &requestError{http.StatusBadRequest, "bad_body", "reading request body: " + err.Error()}
	}
	if len(body) == 0 {
		return &requestError{http.StatusBadRequest, "empty_body", "request body is empty"}
	}
	// JSON is valid YAML, so one strict parser covers both content types.
	if err := yaml.UnmarshalStrict(body, v); err != nil {
		return &requestError{http.StatusBadRequest, "bad_body", "parsing request body: " + err.Error()}
	}
	return nil
}

func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	return mt
}
