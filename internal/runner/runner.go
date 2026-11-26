// Package runner runs every check at once, each with its own timeout and
// retries, and collects the results.
package runner

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/checks"
	"github.com/Kurtis-H/platform-probe/internal/config"
)

// Result is the outcome of one check in one round.
type Result struct {
	Name      string        `json:"name"`
	OK        bool          `json:"ok"`
	Error     string        `json:"error,omitempty"`
	Attempts  int           `json:"attempts"`
	Duration  time.Duration `json:"duration_ns"`
	StartedAt time.Time     `json:"started_at"`
}

// Runner holds the built checkers.
type Runner struct {
	checks   []config.Check
	checkers []checks.Checker
	// sleep waits between retries; tests replace it to run instantly.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

// New builds a checker for every check in the config.
func New(cfg *config.Config, client *http.Client) (*Runner, error) {
	r := &Runner{checks: cfg.Checks, sleep: sleepCtx, now: time.Now}
	for _, c := range cfg.Checks {
		checker, err := checks.New(c, client)
		if err != nil {
			return nil, err
		}
		r.checkers = append(r.checkers, checker)
	}
	return r, nil
}

// RunOnce runs all checks concurrently and returns results sorted by name.
func (r *Runner) RunOnce(ctx context.Context) []Result {
	results := make([]Result, len(r.checks))
	var wg sync.WaitGroup
	for i := range r.checks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = r.runOne(ctx, r.checks[i], r.checkers[i])
		}(i)
	}
	wg.Wait()
	sort.Slice(results, func(a, b int) bool { return results[a].Name < results[b].Name })
	return results
}

// runOne tries a check up to 1+Retries times. Each attempt gets the full
// timeout. Retries back off: 200ms, 400ms, 800ms...
func (r *Runner) runOne(ctx context.Context, c config.Check, checker checks.Checker) Result {
	res := Result{Name: c.Name, StartedAt: r.now()}
	started := time.Now()
	var err error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			if sleepErr := r.sleep(ctx, backoff(attempt)); sleepErr != nil {
				err = sleepErr
				break
			}
		}
		res.Attempts++
		attemptCtx, cancel := context.WithTimeout(ctx, c.Timeout.Duration)
		err = checker.Run(attemptCtx)
		cancel()
		if err == nil {
			break
		}
	}
	res.Duration = time.Since(started)
	res.OK = err == nil
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

func backoff(attempt int) time.Duration {
	return time.Duration(100<<attempt) * time.Millisecond
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// AllOK reports whether every result passed.
func AllOK(results []Result) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}
