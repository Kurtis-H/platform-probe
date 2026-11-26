// Package metrics keeps the latest result of every check and writes them in
// the Prometheus text exposition format, so the existing Grafana dashboard
// can graph them. Written without the Prometheus client library to keep the
// tool dependency-free.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/Kurtis-H/platform-probe/internal/runner"
)

type checkState struct {
	last        runner.Result
	passes      int
	failures    int
	lastSuccess float64 // unix seconds, 0 if never
}

// Registry is safe for concurrent use: the runner records while /metrics reads.
type Registry struct {
	mu     sync.RWMutex
	checks map[string]*checkState
	rounds int
}

func NewRegistry() *Registry { return &Registry{checks: map[string]*checkState{}} }

// Record stores one round of results.
func (r *Registry) Record(results []runner.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rounds++
	for _, res := range results {
		st, ok := r.checks[res.Name]
		if !ok {
			st = &checkState{}
			r.checks[res.Name] = st
		}
		st.last = res
		if res.OK {
			st.passes++
			st.lastSuccess = float64(res.StartedAt.Add(res.Duration).UnixMilli()) / 1000
		} else {
			st.failures++
		}
	}
}

// Latest returns the most recent result of every check, sorted by name.
func (r *Registry) Latest() []runner.Result {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]runner.Result, 0, len(r.checks))
	for _, st := range r.checks {
		out = append(out, st.last)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// WriteTo writes every metric in Prometheus text format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.checks))
	for name := range r.checks {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	metric := func(name, help, kind string, value func(*checkState) string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		for _, n := range names {
			fmt.Fprintf(&b, "%s{check=\"%s\"} %s\n", name, escape(n), value(r.checks[n]))
		}
	}
	metric("probe_success", "1 if the check passed in the last round, else 0.", "gauge",
		func(s *checkState) string { return boolNum(s.last.OK) })
	metric("probe_duration_seconds", "How long the last round of the check took, retries included.", "gauge",
		func(s *checkState) string { return fmt.Sprintf("%g", s.last.Duration.Seconds()) })
	metric("probe_attempts", "Attempts used in the last round (1 = passed or failed first time).", "gauge",
		func(s *checkState) string { return fmt.Sprint(s.last.Attempts) })
	metric("probe_last_success_timestamp_seconds", "Unix time the check last passed (0 = never).", "gauge",
		func(s *checkState) string { return fmt.Sprintf("%g", s.lastSuccess) })

	fmt.Fprintf(&b, "# HELP probe_runs_total Check runs by result.\n# TYPE probe_runs_total counter\n")
	for _, n := range names {
		fmt.Fprintf(&b, "probe_runs_total{check=\"%s\",result=\"pass\"} %d\n", escape(n), r.checks[n].passes)
		fmt.Fprintf(&b, "probe_runs_total{check=\"%s\",result=\"fail\"} %d\n", escape(n), r.checks[n].failures)
	}
	fmt.Fprintf(&b, "# HELP probe_rounds_total Rounds of checks completed.\n# TYPE probe_rounds_total counter\nprobe_rounds_total %d\n", r.rounds)

	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// escape follows the exposition format: backslash, double quote and newline.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func boolNum(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
