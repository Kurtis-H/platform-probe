package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/checks"
	"github.com/Kurtis-H/platform-probe/internal/config"
)

// scripted fails the first `failures` calls, then passes.
type scripted struct {
	failures int32
	calls    atomic.Int32
	delay    time.Duration
}

func (s *scripted) Run(ctx context.Context) error {
	n := s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if n <= s.failures {
		return errors.New("flaky failure")
	}
	return nil
}

func testRunner(checks []config.Check, checkers []checks.Checker) (*Runner, *[]time.Duration) {
	var waits []time.Duration
	return &Runner{
		checks:   checks,
		checkers: checkers,
		sleep:    func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		now:      func() time.Time { return time.Unix(0, 0) },
	}, &waits
}

func check(name string, retries int, timeout time.Duration) config.Check {
	return config.Check{Name: name, Retries: retries, Timeout: config.Duration{Duration: timeout}}
}

func TestRetriesUntilItPasses(t *testing.T) {
	flaky := &scripted{failures: 2}
	r, waits := testRunner([]config.Check{check("flaky", 3, time.Second)}, []checks.Checker{flaky})
	res := r.RunOnce(context.Background())[0]
	if !res.OK || res.Attempts != 3 || res.Error != "" {
		t.Fatalf("got %+v", res)
	}
	if want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}; len(*waits) != 2 || (*waits)[0] != want[0] || (*waits)[1] != want[1] {
		t.Errorf("backoff waits = %v, want %v", *waits, want)
	}
}

func TestGivesUpAfterRetries(t *testing.T) {
	broken := &scripted{failures: 99}
	r, _ := testRunner([]config.Check{check("broken", 2, time.Second)}, []checks.Checker{broken})
	res := r.RunOnce(context.Background())[0]
	if res.OK || res.Attempts != 3 || res.Error != "flaky failure" {
		t.Fatalf("got %+v", res)
	}
}

func TestNoRetriesMeansOneAttempt(t *testing.T) {
	broken := &scripted{failures: 99}
	r, waits := testRunner([]config.Check{check("once", 0, time.Second)}, []checks.Checker{broken})
	if res := r.RunOnce(context.Background())[0]; res.Attempts != 1 || len(*waits) != 0 {
		t.Fatalf("attempts %d waits %v", res.Attempts, *waits)
	}
}

func TestEachAttemptHasItsOwnTimeout(t *testing.T) {
	slow := &scripted{delay: 200 * time.Millisecond}
	r, _ := testRunner([]config.Check{check("slow", 1, 20*time.Millisecond)}, []checks.Checker{slow})
	res := r.RunOnce(context.Background())[0]
	if res.OK || res.Attempts != 2 || res.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("got %+v", res)
	}
}

func TestChecksRunConcurrently(t *testing.T) {
	const n = 5
	var cs []config.Check
	var checkers []checks.Checker
	for i := 0; i < n; i++ {
		cs = append(cs, check(string(rune('a'+i)), 0, time.Second))
		checkers = append(checkers, &scripted{delay: 100 * time.Millisecond})
	}
	r, _ := testRunner(cs, checkers)
	started := time.Now()
	results := r.RunOnce(context.Background())
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("took %s; %d checks of 100ms each should overlap", elapsed, n)
	}
	if !AllOK(results) || results[0].Name != "a" || results[n-1].Name != "e" {
		t.Errorf("results not all OK or not sorted: %+v", results)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	broken := &scripted{failures: 99}
	r := &Runner{
		checks:   []config.Check{check("x", 5, time.Second)},
		checkers: []checks.Checker{broken},
		sleep:    sleepCtx,
		now:      time.Now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.RunOnce(ctx)[0]
	if res.OK || res.Attempts != 1 {
		t.Fatalf("got %+v", res)
	}
}

func TestAllOK(t *testing.T) {
	if !AllOK(nil) || AllOK([]Result{{OK: true}, {OK: false}}) {
		t.Fatal("AllOK is wrong")
	}
}

func TestNewBuildsRealCheckers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := &config.Config{Checks: []config.Check{{
		Name: "up", Type: config.TypeHTTP, Method: "GET", URL: srv.URL, ExpectStatus: 200,
		Timeout: config.Duration{Duration: time.Second},
	}}}
	r, err := New(cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if res := r.RunOnce(context.Background()); !AllOK(res) {
		t.Fatalf("got %+v", res)
	}
	if _, err := New(&config.Config{Checks: []config.Check{{Type: "nope"}}}, srv.Client()); err == nil {
		t.Fatal("expected an error for an unknown type")
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("expected cancellation")
	}
}
