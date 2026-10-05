// Command workloadguard isolates workloads from each other and hardens their pod specs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"

	"github.com/abbyck/workloadguard/internal/api"
	"github.com/abbyck/workloadguard/internal/guard"
	"github.com/abbyck/workloadguard/internal/hardening"
	"github.com/abbyck/workloadguard/internal/isolation"
	"github.com/abbyck/workloadguard/internal/kube"
	"github.com/abbyck/workloadguard/internal/metrics"
)

// version is set at build time: go build -ldflags "-X main.version=0.1.0".
var version = "dev"

type config struct {
	kube            kube.Options
	listen          string
	shutdownTimeout time.Duration
	logLevel        slog.Level
	protected       string
	podCIDRs        string
	cpuRequest      string
	memoryRequest   string
	memoryLimit     string
	rolloutTimeout  time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.kube.Kubeconfig, "kubeconfig", "", "path to a kubeconfig; empty uses in-cluster config, then $KUBECONFIG or ~/.kube/config")
	flag.StringVar(&cfg.kube.Context, "context", "", "kubeconfig context to use; empty uses the current context")
	flag.StringVar(&cfg.listen, "listen", ":8080", "address to serve the HTTP API on")
	flag.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", 20*time.Second, "how long to let in-flight requests finish after SIGTERM")
	flag.StringVar(&cfg.protected, "protected-namespaces", strings.Join(guard.DefaultProtectedNamespaces, ","),
		"comma-separated namespaces the tool never touches; its own namespace is always added")
	flag.StringVar(&cfg.podCIDRs, "pod-cidrs", "10.244.0.0/16",
		"comma-separated CIDRs covering all pod IPs (kind's default is 10.244.0.0/16); isolation excludes them from its ipBlock rule")
	flag.StringVar(&cfg.cpuRequest, "default-cpu-request", "50m", "CPU request set on containers that have none")
	flag.StringVar(&cfg.memoryRequest, "default-memory-request", "64Mi", "memory request set on containers that have none")
	flag.StringVar(&cfg.memoryLimit, "default-memory-limit", "256Mi", "memory limit set on containers that have none (no CPU limit is ever set)")
	flag.DurationVar(&cfg.rolloutTimeout, "rollout-timeout", 90*time.Second, "how long hardening apply waits for patched workloads to roll out; 0 skips the wait")
	flag.TextVar(&cfg.logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.logLevel}))
	if err := run(cfg, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(cfg config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Info("starting", "version", version)
	metrics.RegisterBuildInfo(version)
	metrics.RegisterClientGo()
	client, err := kube.NewClient(cfg.kube)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	podCIDRs, err := isolation.ParsePodCIDRs(splitList(cfg.podCIDRs))
	if err != nil {
		return err
	}
	defaults, err := parseDefaults(cfg)
	if err != nil {
		return err
	}
	g := guard.New(splitList(cfg.protected), ownNamespace())
	log.Info("protected namespaces", "namespaces", g.Protected())

	iso := isolation.NewService(client, g, podCIDRs)
	metrics.RegisterIsolations(func(ctx context.Context) (int, error) {
		list, err := iso.List(ctx)
		return len(list), err
	})

	srv := &http.Server{
		Addr: cfg.listen,
		Handler: api.New(log, apiReachable(client),
			iso,
			hardening.NewService(client, g, defaults, cfg.rolloutTimeout),
		).Handler(),
		// Bounded so slow or stuck clients can't hold connections forever. WriteTimeout
		// leaves room for hardening apply, which waits for rollouts before it responds.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.rolloutTimeout + 90*time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", cfg.listen)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	// Restore default signal handling, so a second Ctrl-C or SIGTERM ends the process at once
	// instead of waiting out the graceful shutdown.
	stop()

	// Stop accepting new connections and let in-flight requests finish, so a rollout or
	// eviction doesn't cut off an isolation or hardening request halfway.
	log.Info("shutting down", "timeout", cfg.shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("stopped")
	return nil
}

// apiReachable checks the API server's /version endpoint, which every authenticated client
// may read, so the readiness probe needs no extra RBAC.
func apiReachable(client kubernetes.Interface) api.ReadyFunc {
	return func(ctx context.Context) error {
		return client.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error()
	}
}

// ownNamespace is where workloadguard runs: POD_NAMESPACE (set from the downward API in the
// deployment), else the service account's namespace, else empty when running locally.
func ownNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parseDefaults(cfg config) (hardening.Defaults, error) {
	var d hardening.Defaults
	for _, q := range []struct {
		flag, value string
		into        *resource.Quantity
	}{
		{"default-cpu-request", cfg.cpuRequest, &d.CPURequest},
		{"default-memory-request", cfg.memoryRequest, &d.MemoryRequest},
		{"default-memory-limit", cfg.memoryLimit, &d.MemoryLimit},
	} {
		v, err := resource.ParseQuantity(q.value)
		if err != nil {
			return d, fmt.Errorf("--%s %q: %w", q.flag, q.value, err)
		}
		*q.into = v
	}
	if d.MemoryRequest.Cmp(d.MemoryLimit) > 0 {
		return d, fmt.Errorf("--default-memory-request %s is above --default-memory-limit %s", &d.MemoryRequest, &d.MemoryLimit)
	}
	return d, nil
}
