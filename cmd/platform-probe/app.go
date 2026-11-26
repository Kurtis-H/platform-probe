package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/config"
	"github.com/Kurtis-H/platform-probe/internal/metrics"
	"github.com/Kurtis-H/platform-probe/internal/runner"
)

const usage = `usage: platform-probe <command> [flags]

commands:
  validate  check a config file
  run       run every check once and print the results (exit 1 on any failure)
  serve     run checks on an interval and serve /metrics, /status and /healthz
`

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "probes.json", "path to the probe config (JSON)")
	listen := fs.String("listen", ":9100", "address for serve to listen on")
	asJSON := fs.Bool("json", false, "run: print results as JSON")
	switch cmd {
	case "validate", "run", "serve":
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config %s:\n%v\n", *configPath, err)
		return exitUsage
	}

	switch cmd {
	case "validate":
		fmt.Fprintf(stdout, "ok: %d checks, every %s\n", len(cfg.Checks), cfg.Interval.Duration)
		return exitOK
	case "run":
		return runOnce(ctx, cfg, stdout, *asJSON)
	default:
		return serve(ctx, cfg, *listen, stderr)
	}
}

func newRunner(cfg *config.Config) (*runner.Runner, error) {
	// Each check has its own context timeout; this is only a backstop.
	return runner.New(cfg, &http.Client{Timeout: 2 * time.Minute})
}

func runOnce(ctx context.Context, cfg *config.Config, out io.Writer, asJSON bool) int {
	r, err := newRunner(cfg)
	if err != nil {
		fmt.Fprintln(out, err)
		return exitUsage
	}
	results := r.RunOnce(ctx)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		printTable(out, results)
	}
	if runner.AllOK(results) {
		return exitOK
	}
	return exitFailed
}

func printTable(out io.Writer, results []runner.Result) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tRESULT\tTIME\tTRIES\tDETAIL")
	failed := 0
	for _, r := range results {
		verdict := "pass"
		if !r.OK {
			verdict = "FAIL"
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.Name, verdict, r.Duration.Round(time.Millisecond), r.Attempts, r.Error)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\n%d of %d checks passed\n", len(results)-failed, len(results))
}

// handler serves the registry. Kept separate from serve() so tests can call it.
func handler(reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = reg.WriteTo(w)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.Latest())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

func serve(ctx context.Context, cfg *config.Config, listen string, logOut io.Writer) int {
	log := slog.New(slog.NewTextHandler(logOut, nil))
	r, err := newRunner(cfg)
	if err != nil {
		log.Error("building checks", "err", err)
		return exitUsage
	}
	reg := metrics.NewRegistry()
	srv := &http.Server{Addr: listen, Handler: handler(reg), ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Error("listening", "addr", listen, "err", err)
		return exitFailed
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
		}
	}()
	log.Info("serving", "addr", ln.Addr().String(), "checks", len(cfg.Checks), "interval", cfg.Interval.Duration)

	round := func() {
		results := r.RunOnce(ctx)
		reg.Record(results)
		for _, res := range results {
			if res.OK {
				log.Info("check passed", "check", res.Name, "duration", res.Duration.Round(time.Millisecond))
			} else {
				log.Warn("check failed", "check", res.Name, "attempts", res.Attempts, "err", res.Error)
			}
		}
	}
	round()
	ticker := time.NewTicker(cfg.Interval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
			log.Info("stopped")
			return exitOK
		case <-ticker.C:
			round()
		}
	}
}
