package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCountCollector(t *testing.T) {
	n, fail := 3, false
	c := &countCollector{
		desc: prometheus.NewDesc("workloadguard_isolations_active", "Isolations currently in the cluster.", nil, nil),
		count: func(context.Context) (int, error) {
			if fail {
				return 0, errors.New("API down")
			}
			return n, nil
		},
	}
	want := "# HELP workloadguard_isolations_active Isolations currently in the cluster.\n" +
		"# TYPE workloadguard_isolations_active gauge\nworkloadguard_isolations_active 3\n"
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
	// A failed count must not be reported as "0 isolations": leave the metric out instead.
	fail = true
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("collected %d metrics while the count fails, want 0", got)
	}
}
