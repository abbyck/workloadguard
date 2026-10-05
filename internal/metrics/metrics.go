// Package metrics holds workloadguard's Prometheus metrics, served on /metrics.
package metrics

import (
	"context"
	"net/url"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

// Registry holds every workloadguard metric plus the Go runtime and process collectors.
// A dedicated registry, not the global one, so only what's listed here is exposed.
var Registry = prometheus.NewRegistry()

var (
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "workloadguard_http_requests_total",
		Help: "API requests by route pattern, method and status code.",
	}, []string{"route", "method", "code"})

	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "workloadguard_http_request_duration_seconds",
		Help: "API request latency by route pattern. Hardening apply includes waiting for rollouts.",
		// Up to 3 minutes: hardening apply waits for rollouts.
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 30, 60, 120, 180},
	}, []string{"route"})

	HardeningWorkloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "workloadguard_hardening_workloads_total",
		Help: "Workloads patched or failed by hardening apply and undo apply.",
	}, []string{"operation", "result"})

	KubeRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "workloadguard_kube_api_requests_total",
		Help: "Kubernetes API requests made by the tool, by HTTP method and status code.",
	}, []string{"method", "code"})

	KubeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "workloadguard_kube_api_request_duration_seconds",
		Help:    "Kubernetes API request latency by verb.",
		Buckets: prometheus.DefBuckets,
	}, []string{"verb"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, HardeningWorkloads, KubeRequests, KubeDuration,
	)
}

// RegisterBuildInfo adds workloadguard_build_info, always 1, with the version as a label, so
// dashboards and alerts can tell which build is running.
func RegisterBuildInfo(version string) {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workloadguard_build_info",
		Help: "Build information; the value is always 1.",
	}, []string{"version", "goversion"})
	info.WithLabelValues(version, runtime.Version()).Set(1)
	Registry.MustRegister(info)
}

// RegisterClientGo routes client-go's request metrics into KubeRequests and KubeDuration.
// Call it once, before creating clients.
func RegisterClientGo() {
	clientmetrics.Register(clientmetrics.RegisterOpts{
		RequestResult:  kubeResult{},
		RequestLatency: kubeLatency{},
	})
}

type kubeResult struct{}

func (kubeResult) Increment(_ context.Context, code, method, _ string) {
	KubeRequests.WithLabelValues(method, code).Inc()
}

type kubeLatency struct{}

func (kubeLatency) Observe(_ context.Context, verb string, _ url.URL, latency time.Duration) {
	KubeDuration.WithLabelValues(verb).Observe(latency.Seconds())
}

// CountFunc returns a current count, such as the number of active isolations.
type CountFunc func(ctx context.Context) (int, error)

// RegisterIsolations adds workloadguard_isolations_active, read from the cluster at scrape
// time. Isolations live in the cluster, not in the tool's memory, so counting them there
// stays right across restarts and manual deletes. If the count fails, the metric is left
// out of that scrape rather than reported as 0.
func RegisterIsolations(count CountFunc) {
	Registry.MustRegister(&countCollector{
		desc:  prometheus.NewDesc("workloadguard_isolations_active", "Isolations currently in the cluster.", nil, nil),
		count: count,
	})
}

type countCollector struct {
	desc  *prometheus.Desc
	count CountFunc
}

func (c *countCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *countCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := c.count(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n))
	}
}
