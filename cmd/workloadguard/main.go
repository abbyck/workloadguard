// Command workloadguard isolates workloads from each other and hardens their pod specs.
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/abbyck/workloadguard/internal/kube"
)

func main() {
	var opts kube.Options
	flag.StringVar(&opts.Kubeconfig, "kubeconfig", "", "path to a kubeconfig; empty uses in-cluster config, then $KUBECONFIG or ~/.kube/config")
	flag.StringVar(&opts.Context, "context", "", "kubeconfig context to use; empty uses the current context")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	client, err := kube.NewClient(opts)
	if err != nil {
		log.Error("create kubernetes client", "err", err)
		os.Exit(1)
	}

	version, err := client.Discovery().ServerVersion()
	if err != nil {
		log.Error("reach kubernetes API server", "err", err)
		os.Exit(1)
	}
	log.Info("connected to kubernetes", "serverVersion", version.GitVersion)
}
