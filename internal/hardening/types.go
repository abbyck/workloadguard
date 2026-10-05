// Package hardening fills in missing resource requests/limits and securityContext fields on
// workloads' pod templates.
package hardening

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Level selects which securityContext fields are filled in.
type Level string

const (
	// Baseline only sets fields that don't change what a normal process can do at startup:
	// allowPrivilegeEscalation=false and the RuntimeDefault seccomp profile.
	Baseline Level = "baseline"
	// Strict also drops all capabilities, requires a non-root user and makes the root
	// filesystem read-only. Each of these can break an image, so it's opt-in.
	Strict Level = "strict"
)

// Request asks for the workloads in Namespaces to be hardened at Level.
type Request struct {
	Namespaces []string `json:"namespaces"`
	Level      Level    `json:"level"`
}

// Defaults are the resource values filled in where a container has none.
type Defaults struct {
	CPURequest    resource.Quantity
	MemoryRequest resource.Quantity
	MemoryLimit   resource.Quantity
}

// Change is one field the tool sets. Before is nil: the tool only fills unset fields.
// In an undo plan it's the reverse: Before is the tool's value and After is nil.
type Change struct {
	// Container is the container name, or "" for a pod-level field.
	Container string `json:"container"`
	Field     string `json:"field"`
	Before    any    `json:"before"`
	After     any    `json:"after"`
}

// WorkloadRef names a workload.
type WorkloadRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (w WorkloadRef) String() string { return w.Kind + " " + w.Namespace + "/" + w.Name }

// Apply results.
const (
	ResultPatched   = "patched"
	ResultUnchanged = "unchanged"
	ResultFailed    = "failed"
)

// WorkloadPlan is what hardening does (plan) or did (apply) to one workload.
type WorkloadPlan struct {
	WorkloadRef
	// RestartsPods is true when the pod template changes, which triggers a rolling restart.
	RestartsPods bool     `json:"restartsPods"`
	Changes      []Change `json:"changes"`
	// Notes are things the tool deliberately left alone, or risks of the chosen level.
	Notes []string `json:"notes,omitempty"`

	// Set by apply.
	Result  string `json:"result,omitempty"`
	Rollout string `json:"rollout,omitempty"`
	// Error is set when the API server rejected the patch (in plan: on server-side dry run).
	Error string `json:"error,omitempty"`
}

// Skipped is a workload the tool looked at but won't patch.
type Skipped struct {
	WorkloadRef
	Reason string `json:"reason"`
}

// Plan is the result of planning or applying hardening.
type Plan struct {
	Level Level `json:"level,omitempty"`
	// Undo is true for an undo plan: each change's before is what the tool set, after is
	// nil (removed).
	Undo      bool           `json:"undo,omitempty"`
	DryRun    bool           `json:"dryRun"`
	Workloads []WorkloadPlan `json:"workloads"`
	Skipped   []Skipped      `json:"skipped"`
	// Unchanged lists workloads that already meet the level.
	Unchanged []WorkloadRef `json:"unchanged"`
	Notes     []string      `json:"notes"`
}

// InvalidError lists everything wrong with a request.
type InvalidError struct {
	Problems []string
}

func (e *InvalidError) Error() string {
	return "invalid hardening request: " + strings.Join(e.Problems, "; ")
}

// NamespaceNotFoundError means a requested namespace doesn't exist.
type NamespaceNotFoundError struct {
	Namespace string
}

func (e *NamespaceNotFoundError) Error() string {
	return fmt.Sprintf("namespace %q not found", e.Namespace)
}

// normalize validates the request and fills in the default level.
func (r Request) normalize() (Request, error) {
	var problems []string
	if len(r.Namespaces) == 0 {
		problems = append(problems, "namespaces must list at least one namespace")
	}
	seen := map[string]bool{}
	for _, ns := range r.Namespaces {
		for _, msg := range validation.IsDNS1123Label(ns) {
			problems = append(problems, fmt.Sprintf("namespace %q: %s", ns, msg))
		}
		if seen[ns] {
			problems = append(problems, fmt.Sprintf("namespace %q listed twice", ns))
		}
		seen[ns] = true
	}
	if r.Level == "" {
		r.Level = Baseline
	}
	if r.Level != Baseline && r.Level != Strict {
		problems = append(problems, fmt.Sprintf("level %q must be %q or %q", r.Level, Baseline, Strict))
	}
	if len(problems) > 0 {
		return r, &InvalidError{Problems: problems}
	}
	r.Namespaces = slices.Clone(r.Namespaces)
	return r, nil
}
