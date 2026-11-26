package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/metrics"
	"github.com/Kurtis-H/platform-probe/internal/runner"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probes.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cli(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(ctx, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func api(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, `{"status": "ok"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func twoChecks(url string) string {
	return fmt.Sprintf(`{"interval": "50ms", "checks": [
		{"name": "health", "type": "http", "url": "%[1]s/health", "json": [{"path": "status", "equals": "ok"}]},
		{"name": "missing page", "type": "http", "url": "%[1]s/nope"}
	]}`, url)
}

func TestUsage(t *testing.T) {
	if code, _, errOut := cli(t, context.Background()); code != exitUsage || !strings.Contains(errOut, "usage") {
		t.Errorf("no args: %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, context.Background(), "explode"); code != exitUsage || !strings.Contains(errOut, `unknown command "explode"`) {
		t.Errorf("bad command: %d %q", code, errOut)
	}
	if code, out, _ := cli(t, context.Background(), "help"); code != exitOK || !strings.Contains(out, "commands:") {
		t.Errorf("help: %d %q", code, out)
	}
	if code, _, _ := cli(t, context.Background(), "run", "-nope"); code != exitUsage {
		t.Errorf("bad flag: %d", code)
	}
}

func TestValidate(t *testing.T) {
	good := writeConfig(t, twoChecks("http://x"))
	if code, out, _ := cli(t, context.Background(), "validate", "-config", good); code != exitOK || !strings.Contains(out, "ok: 2 checks, every 50ms") {
		t.Errorf("good config: %d %q", code, out)
	}
	bad := writeConfig(t, `{"checks": [{"name": "x", "type": "ping"}]}`)
	if code, _, errOut := cli(t, context.Background(), "validate", "-config", bad); code != exitUsage || !strings.Contains(errOut, `unknown type "ping"`) {
		t.Errorf("bad config: %d %q", code, errOut)
	}
}

func TestRunPrintsTableAndFailsOnAnyFailure(t *testing.T) {
	path := writeConfig(t, twoChecks(api(t).URL))
	code, out, _ := cli(t, context.Background(), "run", "-config", path)
	if code != exitFailed {
		t.Errorf("exit = %d, want %d", code, exitFailed)
	}
	for _, want := range []string{"CHECK", "health", "pass", "missing page", "FAIL", "status 404, want 200", "1 of 2 checks passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRunJSONAllPassing(t *testing.T) {
	srv := api(t)
	path := writeConfig(t, fmt.Sprintf(`{"checks": [{"name": "health", "type": "http", "url": "%s/health"}]}`, srv.URL))
	code, out, _ := cli(t, context.Background(), "run", "-config", path, "-json")
	var results []runner.Result
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != exitOK || len(results) != 1 || !results[0].OK {
		t.Errorf("code %d results %+v", code, results)
	}
}

func TestHandler(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.Record([]runner.Result{{Name: "api", OK: true, Attempts: 1}})
	srv := httptest.NewServer(handler(reg))
	defer srv.Close()

	body := func(path string) (int, string, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	if code, ctype, b := body("/metrics"); code != 200 || !strings.HasPrefix(ctype, "text/plain") || !strings.Contains(b, `probe_success{check="api"} 1`) {
		t.Errorf("/metrics: %d %q %q", code, ctype, b)
	}
	if code, _, b := body("/status"); code != 200 || !strings.Contains(b, `"name":"api"`) {
		t.Errorf("/status: %d %q", code, b)
	}
	if code, _, b := body("/healthz"); code != 200 || b != "ok\n" {
		t.Errorf("/healthz: %d %q", code, b)
	}
	if code, _, _ := body("/nope"); code != 404 {
		t.Errorf("/nope: %d", code)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestServeRunsRoundsAndStopsCleanly(t *testing.T) {
	path := writeConfig(t, twoChecks(api(t).URL))
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	var logs bytes.Buffer
	go func() { done <- run(ctx, []string{"serve", "-config", path, "-listen", addr}, io.Discard, &logs) }()

	deadline := time.Now().Add(5 * time.Second)
	var metricsText string
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/metrics"); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			metricsText = string(b)
			if strings.Contains(metricsText, "probe_rounds_total 3") {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("serve exit = %d", code)
	}
	if !strings.Contains(metricsText, `probe_success{check="health"} 1`) || !strings.Contains(metricsText, `probe_success{check="missing page"} 0`) {
		t.Errorf("metrics after several rounds:\n%s", metricsText)
	}
}

func TestServeReportsAPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	path := writeConfig(t, twoChecks("http://127.0.0.1:1"))
	if code, _, errOut := cli(t, context.Background(), "serve", "-config", path, "-listen", ln.Addr().String()); code != exitFailed || !strings.Contains(errOut, "listening") {
		t.Errorf("code %d log %q", code, errOut)
	}
}
