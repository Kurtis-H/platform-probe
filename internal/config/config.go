// Package config loads and validates probe definitions from JSON.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// Check types.
const (
	TypeHTTP            = "http"
	TypeBookingPurchase = "booking_purchase"
)

// Duration accepts "250ms", "5s", "1m" in JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q is negative", s)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Assertion checks one value in a JSON response body.
type Assertion struct {
	Path     string `json:"path"`
	Equals   any    `json:"equals,omitempty"`
	Exists   *bool  `json:"exists,omitempty"`
	MinItems *int   `json:"min_items,omitempty"`
}

// Check is one probe.
type Check struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Timeout Duration `json:"timeout"`
	Retries int      `json:"retries"`

	// http
	Method       string            `json:"method,omitempty"`
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	ExpectStatus int               `json:"expect_status,omitempty"`
	MaxLatency   Duration          `json:"max_latency"`
	JSON         []Assertion       `json:"json,omitempty"`

	// booking_purchase
	BaseURL  string `json:"base_url,omitempty"`
	AdminKey string `json:"admin_key,omitempty"`
	BuyerKey string `json:"buyer_key,omitempty"`
	OtherKey string `json:"other_key,omitempty"`
}

// Config is a whole probe file.
type Config struct {
	Interval Duration `json:"interval"`
	Checks   []Check  `json:"checks"`
}

const (
	defaultTimeout  = 10 * time.Second
	defaultInterval = time.Minute
	maxRetries      = 5
)

// Load reads and validates a config file. Values like "${API_URL}" are
// replaced from the environment, so one file can target different hosts.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse decodes, applies defaults and validates. Unknown fields are errors,
// so a typo like "expect_staus" fails loudly instead of being ignored.
func Parse(r io.Reader) (*Config, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(os.ExpandEnv(string(raw))))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Interval.Duration == 0 {
		c.Interval.Duration = defaultInterval
	}
	for i := range c.Checks {
		ch := &c.Checks[i]
		if ch.Timeout.Duration == 0 {
			ch.Timeout.Duration = defaultTimeout
		}
		if ch.Type == TypeHTTP {
			if ch.Method == "" {
				ch.Method = "GET"
			}
			ch.Method = strings.ToUpper(ch.Method)
			if ch.ExpectStatus == 0 {
				ch.ExpectStatus = 200
			}
		}
	}
}

// Validate reports every problem at once, so a broken file can be fixed in one go.
func (c *Config) Validate() error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	if len(c.Checks) == 0 {
		add("no checks defined")
	}
	seen := map[string]bool{}
	for i, ch := range c.Checks {
		where := fmt.Sprintf("check %d (%q)", i+1, ch.Name)
		if strings.TrimSpace(ch.Name) == "" {
			add("check %d: name is required", i+1)
		} else if seen[ch.Name] {
			add("%s: duplicate name", where)
		}
		seen[ch.Name] = true
		if ch.Retries < 0 || ch.Retries > maxRetries {
			add("%s: retries must be between 0 and %d", where, maxRetries)
		}
		switch ch.Type {
		case TypeHTTP:
			if !validURL(ch.URL) {
				add("%s: url must be an absolute http(s) URL", where)
			}
			if ch.ExpectStatus < 100 || ch.ExpectStatus > 599 {
				add("%s: expect_status %d is not an HTTP status", where, ch.ExpectStatus)
			}
			for j, a := range ch.JSON {
				if a.Equals == nil && a.Exists == nil && a.MinItems == nil {
					add("%s: json assertion %d needs equals, exists or min_items", where, j+1)
				}
			}
		case TypeBookingPurchase:
			if !validURL(ch.BaseURL) {
				add("%s: base_url must be an absolute http(s) URL", where)
			}
			if ch.AdminKey == "" || ch.BuyerKey == "" {
				add("%s: admin_key and buyer_key are required", where)
			}
			if ch.OtherKey != "" && ch.OtherKey == ch.BuyerKey {
				add("%s: other_key must be a different buyer from buyer_key", where)
			}
		default:
			add("%s: unknown type %q (want %q or %q)", where, ch.Type, TypeHTTP, TypeBookingPurchase)
		}
	}
	return errors.Join(problems...)
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
