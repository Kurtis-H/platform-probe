package config

import (
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, s string) (*Config, error) {
	t.Helper()
	return Parse(strings.NewReader(s))
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := parse(t, `{"checks": [{"name": "events", "type": "http", "url": "http://api/events", "method": "get"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	ch := cfg.Checks[0]
	if cfg.Interval.Duration != time.Minute || ch.Timeout.Duration != 10*time.Second {
		t.Errorf("defaults not applied: interval %v timeout %v", cfg.Interval, ch.Timeout)
	}
	if ch.Method != "GET" || ch.ExpectStatus != 200 {
		t.Errorf("http defaults not applied: %q %d", ch.Method, ch.ExpectStatus)
	}
}

func TestParseDurations(t *testing.T) {
	cfg, err := parse(t, `{"interval": "30s", "checks": [{"name": "a", "type": "http", "url": "https://x/", "timeout": "250ms", "max_latency": "1.5s"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	ch := cfg.Checks[0]
	if cfg.Interval.Duration != 30*time.Second || ch.Timeout.Duration != 250*time.Millisecond || ch.MaxLatency.Duration != 1500*time.Millisecond {
		t.Errorf("got %v %v %v", cfg.Interval, ch.Timeout, ch.MaxLatency)
	}
}

func TestParseExpandsEnvironment(t *testing.T) {
	t.Setenv("API_URL", "http://booking:8000")
	cfg, err := parse(t, `{"checks": [{"name": "a", "type": "http", "url": "${API_URL}/health"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Checks[0].URL; got != "http://booking:8000/health" {
		t.Errorf("url = %q", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, config, wantErr string
	}{
		{"typo in a field", `{"checks": [{"name": "a", "type": "http", "url": "http://x", "expect_staus": 200}]}`, "unknown field"},
		{"bad duration", `{"interval": "soon", "checks": []}`, "invalid duration"},
		{"numeric duration", `{"interval": 5, "checks": []}`, "must be a string"},
		{"negative duration", `{"interval": "-1s", "checks": []}`, "negative"},
		{"no checks", `{"checks": []}`, "no checks defined"},
		{"missing name", `{"checks": [{"type": "http", "url": "http://x"}]}`, "name is required"},
		{"duplicate name", `{"checks": [{"name": "a", "type": "http", "url": "http://x"}, {"name": "a", "type": "http", "url": "http://x"}]}`, "duplicate name"},
		{"unknown type", `{"checks": [{"name": "a", "type": "ping"}]}`, `unknown type "ping"`},
		{"relative url", `{"checks": [{"name": "a", "type": "http", "url": "/events"}]}`, "absolute http(s) URL"},
		{"ftp url", `{"checks": [{"name": "a", "type": "http", "url": "ftp://x/y"}]}`, "absolute http(s) URL"},
		{"bad status", `{"checks": [{"name": "a", "type": "http", "url": "http://x", "expect_status": 999}]}`, "not an HTTP status"},
		{"too many retries", `{"checks": [{"name": "a", "type": "http", "url": "http://x", "retries": 9}]}`, "retries must be between"},
		{"empty assertion", `{"checks": [{"name": "a", "type": "http", "url": "http://x", "json": [{"path": "total"}]}]}`, "needs equals, exists or min_items"},
		{"journey without keys", `{"checks": [{"name": "j", "type": "booking_purchase", "base_url": "http://x"}]}`, "admin_key and buyer_key"},
		{"journey same buyers", `{"checks": [{"name": "j", "type": "booking_purchase", "base_url": "http://x", "admin_key": "a", "buyer_key": "b", "other_key": "b"}]}`, "different buyer"},
		{"not json", `checks: []`, "reading config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := parse(t, `{"checks": [{"type": "http", "url": "/x", "retries": -1}]}`)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"name is required", "absolute http(s) URL", "retries must be between"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("does-not-exist.json"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	b, err := Duration{1500 * time.Millisecond}.MarshalJSON()
	if err != nil || string(b) != `"1.5s"` {
		t.Fatalf("got %s, %v", b, err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(`{"checks": [{"name": "a", "type": "http", "url": "http://x"}]}`)
	f.Add(`{"interval": "1s", "checks": [{"name": "j", "type": "booking_purchase", "base_url": "http://x", "admin_key": "a", "buyer_key": "b"}]}`)
	f.Fuzz(func(t *testing.T, s string) {
		cfg, err := Parse(strings.NewReader(s))
		if err == nil && (cfg == nil || len(cfg.Checks) == 0) {
			t.Fatalf("accepted a config with no checks: %q", s)
		}
	})
}
