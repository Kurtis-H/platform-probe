package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/runner"
)

func text(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestMetricsAfterTwoRounds(t *testing.T) {
	r := NewRegistry()
	start := time.Unix(1_800_000_000, 0)
	r.Record([]runner.Result{
		{Name: "api", OK: true, Attempts: 1, Duration: 250 * time.Millisecond, StartedAt: start},
		{Name: "journey", OK: false, Attempts: 3, Duration: 2 * time.Second, StartedAt: start},
	})
	r.Record([]runner.Result{
		{Name: "api", OK: false, Attempts: 2, Duration: time.Second, StartedAt: start.Add(time.Minute)},
		{Name: "journey", OK: true, Attempts: 1, Duration: 500 * time.Millisecond, StartedAt: start.Add(time.Minute)},
	})
	out := text(t, r)
	for _, want := range []string{
		"# TYPE probe_success gauge",
		`probe_success{check="api"} 0`,
		`probe_success{check="journey"} 1`,
		`probe_duration_seconds{check="api"} 1`,
		`probe_attempts{check="api"} 2`,
		`probe_last_success_timestamp_seconds{check="api"} 1.80000000025e+09`,
		`probe_last_success_timestamp_seconds{check="journey"} 1.8000000605e+09`,
		"# TYPE probe_runs_total counter",
		`probe_runs_total{check="api",result="pass"} 1`,
		`probe_runs_total{check="api",result="fail"} 1`,
		"probe_rounds_total 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, `check="api"`) > strings.Index(out, `check="journey"`) {
		t.Error("checks should be sorted by name")
	}
}

func TestNeverPassedIsZero(t *testing.T) {
	r := NewRegistry()
	r.Record([]runner.Result{{Name: "down", OK: false}})
	if out := text(t, r); !strings.Contains(out, `probe_last_success_timestamp_seconds{check="down"} 0`) {
		t.Errorf("got:\n%s", out)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := NewRegistry()
	r.Record([]runner.Result{{Name: "a \"quoted\" \\ name\nnext", OK: true}})
	if out := text(t, r); !strings.Contains(out, `probe_success{check="a \"quoted\" \\ name\nnext"} 1`) {
		t.Errorf("not escaped:\n%s", out)
	}
}

func TestLatestIsSorted(t *testing.T) {
	r := NewRegistry()
	r.Record([]runner.Result{{Name: "b"}, {Name: "a"}})
	got := r.Latest()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestConcurrentRecordAndRead(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.Record([]runner.Result{{Name: "x", OK: true}}) }()
		go func() { defer wg.Done(); _ = text(t, r); _ = r.Latest() }()
	}
	wg.Wait()
	if !strings.Contains(text(t, r), "probe_rounds_total 20") {
		t.Fatal("lost rounds")
	}
}
