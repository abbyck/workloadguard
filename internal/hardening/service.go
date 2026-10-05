package hardening

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/abbyck/workloadguard/internal/guard"
)

// Annotations on patched workloads. ChangesAnnotation records every field the tool set,
// with its previous value, which is what an undo would need (see README).
const (
	HardenedAtAnnotation = "workloadguard.io/hardened-at"
	LevelAnnotation      = "workloadguard.io/hardening-level"
	ChangesAnnotation    = "workloadguard.io/hardening-changes"
)

const fieldManager = "workloadguard"

// Service plans and applies hardening.
type Service struct {
	client         kubernetes.Interface
	guard          *guard.Guard
	defaults       Defaults
	rolloutTimeout time.Duration
	pollInterval   time.Duration
	// settle is how long to watch pods after a rollout reports done. A container without a
	// readiness probe counts as available the moment it starts, so an image that crashes
	// a second later still "completes" its rollout, and the old pods are already gone.
	settle time.Duration
	now    func() time.Time
}

// NewService returns a Service. rolloutTimeout is how long apply waits for patched
// workloads to roll out; 0 skips the wait.
func NewService(client kubernetes.Interface, g *guard.Guard, d Defaults, rolloutTimeout time.Duration) *Service {
	return &Service{client: client, guard: g, defaults: d, rolloutTimeout: rolloutTimeout,
		pollInterval: 2 * time.Second, settle: 10 * time.Second, now: time.Now}
}

// RolloutTimeout is how long Apply may wait for rollouts.
func (s *Service) RolloutTimeout() time.Duration { return s.rolloutTimeout }

// Plan returns what Apply would do, without changing anything. Each patch is also sent as
// a server-side dry run, so API validation and admission get their say first.
func (s *Service) Plan(ctx context.Context, r Request) (*Plan, error) {
	return s.run(ctx, r, true)
}

// Apply patches the workloads and waits for their rollouts. It works the plan out again
// from the live objects rather than trusting an earlier dry run, so it reports what it
// actually changed. It carries on past individual failures and reports each workload.
func (s *Service) Apply(ctx context.Context, r Request) (*Plan, error) {
	return s.run(ctx, r, false)
}

func (s *Service) run(ctx context.Context, r Request, dryRun bool) (*Plan, error) {
	r, err := r.normalize()
	if err != nil {
		return nil, err
	}
	// Check every namespace before touching any, so a bad entry doesn't leave the request
	// half applied.
	for _, ns := range r.Namespaces {
		if err := s.checkNamespace(ctx, ns); err != nil {
			return nil, err
		}
	}

	plan := &Plan{Level: r.Level, DryRun: dryRun,
		Workloads: []WorkloadPlan{}, Skipped: []Skipped{}, Unchanged: []WorkloadRef{}, Notes: []string{}}
	for _, ns := range r.Namespaces {
		if err := s.namespace(ctx, ns, r.Level, dryRun, plan); err != nil {
			return nil, err
		}
	}
	if !dryRun {
		s.waitForRollouts(ctx, plan)
	}
	return plan, nil
}

func (s *Service) checkNamespace(ctx context.Context, name string) error {
	ns, err := s.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &NamespaceNotFoundError{Namespace: name}
	}
	if err != nil {
		return fmt.Errorf("get namespace %s: %w", name, err)
	}
	return s.guard.CheckNamespace(ns)
}

func (s *Service) namespace(ctx context.Context, ns string, level Level, dryRun bool, plan *Plan) error {
	skipResources, err := s.hasLimitRangeDefaults(ctx, ns)
	if err != nil {
		return err
	}
	if skipResources {
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"namespace %s has a LimitRange with container defaults: resources left to it", ns))
	}

	for _, k := range patchable {
		items, err := k.list(ctx, s.client, ns)
		if err != nil {
			return fmt.Errorf("list %ss in %s: %w", k.name, ns, err)
		}
		for _, o := range items {
			ref := WorkloadRef{Kind: k.name, Namespace: o.GetNamespace(), Name: o.GetName()}
			if err := s.guard.CheckWorkload(k.name, o); err != nil {
				plan.Skipped = append(plan.Skipped, Skipped{WorkloadRef: ref, Reason: err.Error()})
				continue
			}
			if wp := s.workload(ctx, k, o, level, skipResources, dryRun); wp != nil {
				plan.Workloads = append(plan.Workloads, *wp)
			} else {
				plan.Unchanged = append(plan.Unchanged, ref)
			}
		}
	}
	return s.reportOnly(ctx, ns, level, skipResources, plan)
}

// workload plans or applies one workload. It returns nil if nothing needs changing.
func (s *Service) workload(ctx context.Context, k kind, o object, level Level, skipResources, dryRun bool) *WorkloadPlan {
	wp := &WorkloadPlan{WorkloadRef: WorkloadRef{Kind: k.name, Namespace: o.GetNamespace(), Name: o.GetName()}}
	opts := metav1.PatchOptions{FieldManager: fieldManager}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}

	// On a conflict (someone changed the workload since we read it), read it again and
	// recompute, so a concurrent edit is never overwritten with stale values.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := o
		if !dryRun {
			var err error
			if cur, err = k.get(ctx, s.client, o.GetNamespace(), o.GetName()); err != nil {
				return err
			}
		}
		changes, notes, patch, err := s.buildPatch(k, cur, level, skipResources)
		wp.Changes, wp.Notes = changes, notes
		if err != nil || len(changes) == 0 {
			return err
		}
		_, err = k.patch(ctx, s.client, cur.GetNamespace(), cur.GetName(), patch, opts)
		return err
	})

	if len(wp.Changes) == 0 && err == nil {
		return nil
	}
	wp.RestartsPods = len(wp.Changes) > 0
	switch {
	case err != nil:
		wp.Error = err.Error()
		if !dryRun {
			wp.Result = ResultFailed
		}
	case !dryRun:
		wp.Result = ResultPatched
	}
	return wp
}

// buildPatch hardens a copy of o and returns the changes and a strategic merge patch that
// makes them. The patch carries o's resourceVersion, so the API server rejects it with a
// conflict if o changed in the meantime.
func (s *Service) buildPatch(k kind, o object, level Level, skipResources bool) ([]Change, []string, []byte, error) {
	modified := o.DeepCopyObject().(object)
	changes, notes := hardenPodSpec(&template(modified).Spec, level, s.defaults, skipResources)
	if len(changes) == 0 {
		return nil, notes, nil, nil
	}

	all := changes
	if prev := o.GetAnnotations()[ChangesAnnotation]; prev != "" {
		var earlier []Change
		if err := json.Unmarshal([]byte(prev), &earlier); err == nil {
			all = append(earlier, changes...)
		}
	}
	record, err := json.Marshal(all)
	if err != nil {
		return nil, nil, nil, err
	}
	ann := modified.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[HardenedAtAnnotation] = s.now().UTC().Format(time.RFC3339)
	ann[LevelAnnotation] = string(level)
	ann[ChangesAnnotation] = string(record)
	modified.SetAnnotations(ann)

	before, err := json.Marshal(o)
	if err != nil {
		return nil, nil, nil, err
	}
	after, err := json.Marshal(modified)
	if err != nil {
		return nil, nil, nil, err
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(before, after, k.schema)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("compute patch: %w", err)
	}
	patch, err = withResourceVersion(patch, o.GetResourceVersion())
	return changes, notes, patch, err
}

func withResourceVersion(patch []byte, rv string) ([]byte, error) {
	if rv == "" {
		return patch, nil
	}
	var m map[string]any
	if err := json.Unmarshal(patch, &m); err != nil {
		return nil, err
	}
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		m["metadata"] = meta
	}
	meta["resourceVersion"] = rv
	return json.Marshal(m)
}

func (s *Service) hasLimitRangeDefaults(ctx context.Context, ns string) (bool, error) {
	lrs, err := s.client.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list LimitRanges in %s: %w", ns, err)
	}
	for _, lr := range lrs.Items {
		for _, l := range lr.Spec.Limits {
			if l.Type == corev1.LimitTypeContainer && (len(l.Default) > 0 || len(l.DefaultRequest) > 0) {
				return true, nil
			}
		}
	}
	return false, nil
}

// reportOnly lists workloads the tool doesn't patch but that would need changes.
func (s *Service) reportOnly(ctx context.Context, ns string, level Level, skipResources bool, plan *Plan) error {
	report := func(kind, name string, spec corev1.PodSpec, reason string) {
		if changes, _ := hardenPodSpec(&spec, level, s.defaults, skipResources); len(changes) > 0 {
			plan.Skipped = append(plan.Skipped, Skipped{WorkloadRef: WorkloadRef{Kind: kind, Namespace: ns, Name: name}, Reason: reason})
		}
	}

	pods, err := s.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods in %s: %w", ns, err)
	}
	for _, p := range pods.Items {
		if len(p.OwnerReferences) == 0 {
			report("Pod", p.Name, p.Spec, "bare Pod: reported, not patched. Most securityContext fields can't change "+
				"on a running pod and nothing would recreate it; fix its manifest")
		}
	}
	jobs, err := s.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Jobs in %s: %w", ns, err)
	}
	for _, j := range jobs.Items {
		if len(j.OwnerReferences) == 0 {
			report("Job", j.Name, j.Spec.Template.Spec, "Job: reported, not patched. A Job's pod template can't be changed")
		}
	}
	cronJobs, err := s.client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list CronJobs in %s: %w", ns, err)
	}
	for _, c := range cronJobs.Items {
		report("CronJob", c.Name, c.Spec.JobTemplate.Spec.Template.Spec,
			"CronJob: reported, not patched. Out of scope for this tool; fix its manifest")
	}
	return nil
}

// waitForRollouts waits, concurrently, for every patched workload to roll out or for the
// timeout, and records the outcome. A crash-looping image shows up here instead of as a
// silently stuck rollout.
func (s *Service) waitForRollouts(ctx context.Context, plan *Plan) {
	var wg sync.WaitGroup
	for i := range plan.Workloads {
		wp := &plan.Workloads[i]
		if wp.Result != ResultPatched {
			continue
		}
		if s.rolloutTimeout == 0 {
			wp.Rollout = "not checked"
			continue
		}
		wg.Go(func() { wp.Rollout = s.waitForRollout(ctx, wp.WorkloadRef) })
	}
	wg.Wait()
}

func (s *Service) waitForRollout(ctx context.Context, ref WorkloadRef) string {
	var k kind
	for _, pk := range patchable {
		if pk.name == ref.Kind {
			k = pk
		}
	}
	var last object
	msg := "no status read yet"
	err := wait.PollUntilContextTimeout(ctx, s.pollInterval, s.rolloutTimeout, true, func(ctx context.Context) (bool, error) {
		o, err := k.get(ctx, s.client, ref.Namespace, ref.Name)
		if err != nil {
			// Keep the last real status: the final poll often fails only because the
			// deadline is up, and that says nothing about the workload.
			return false, nil
		}
		last = o
		done, final, m := rolloutStatus(o)
		msg = m
		if !done && final {
			return false, errFinal
		}
		return done, nil
	})
	switch {
	case err == nil:
		if problems := s.settledProblems(ctx, last); problems != "" {
			return "rolled out, but pods are failing: " + problems
		}
		return "ready"
	case errors.Is(err, errFinal):
		return msg
	}
	out := fmt.Sprintf("not ready after %s: %s", s.rolloutTimeout, msg)
	if last != nil {
		if problems := s.podProblems(ctx, last); problems != "" {
			out += "; " + problems
		}
	}
	return out
}

// settledProblems waits for the settle period, then checks the workload's pods.
func (s *Service) settledProblems(ctx context.Context, o object) string {
	if s.settle > 0 {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(s.settle):
		}
	}
	return s.podProblems(ctx, o)
}

var errFinal = errors.New("rollout won't progress on its own")

// podProblems summarizes containers of the workload's pods that are failing: stuck waiting
// (CrashLoopBackOff, or CreateContainerConfigError, which is what runAsNonRoot gives a root
// image), exited with an error, or restarted.
func (s *Service) podProblems(ctx context.Context, o object) string {
	sel, err := metav1.LabelSelectorAsSelector(selector(o))
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	pods, err := s.client.CoreV1().Pods(o.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return ""
	}
	var problems []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue // old pod on its way out
		}
		for _, st := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			if problem := containerProblem(st); problem != "" {
				problems = append(problems, fmt.Sprintf("pod %s container %s: %s", p.Name, st.Name, problem))
			}
		}
	}
	return strings.Join(problems, ", ")
}

func containerProblem(st corev1.ContainerStatus) string {
	switch {
	case st.State.Waiting != nil && st.State.Waiting.Reason != "ContainerCreating" && st.State.Waiting.Reason != "PodInitializing":
		return st.State.Waiting.Reason
	case st.State.Terminated != nil && st.State.Terminated.ExitCode != 0:
		return fmt.Sprintf("%s (exit %d)", st.State.Terminated.Reason, st.State.Terminated.ExitCode)
	case st.RestartCount > 0:
		return fmt.Sprintf("restarted %d times", st.RestartCount)
	}
	return ""
}
