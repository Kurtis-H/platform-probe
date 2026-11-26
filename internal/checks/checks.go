// Package checks runs one probe: an HTTP request with assertions, or a full
// purchase journey against the booking API.
package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/config"
	"github.com/Kurtis-H/platform-probe/internal/jsonpath"
)

// NewClient returns the HTTP client probes should use. It follows redirects
// only within the same host: Go keeps custom headers such as X-API-Key when
// it follows a redirect, so a redirect to another host would hand that host
// the probe's keys.
func NewClient(backstop time.Duration) *http.Client {
	return &http.Client{
		Timeout: backstop,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing to follow a redirect from %s to %s", via[0].URL.Host, req.URL.Host)
			}
			return nil
		},
	}
}

// Checker runs a check once. A nil error means it passed.
type Checker interface {
	Run(ctx context.Context) error
}

// New builds the checker for a config entry.
func New(c config.Check, client *http.Client) (Checker, error) {
	switch c.Type {
	case config.TypeHTTP:
		return &HTTPCheck{cfg: c, client: client}, nil
	case config.TypeBookingPurchase:
		return &BookingPurchase{cfg: c, client: client, now: time.Now}, nil
	default:
		return nil, fmt.Errorf("unknown check type %q", c.Type)
	}
}

// HTTPCheck sends one request and checks status, latency and JSON values.
type HTTPCheck struct {
	cfg    config.Check
	client *http.Client
}

// maxBody caps how much of a response is read, so a huge page can't eat memory.
const maxBody = 4 << 20

func (h *HTTPCheck) Run(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, h.cfg.Method, h.cfg.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range h.cfg.Headers {
		req.Header.Set(k, v)
	}
	started := time.Now()
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	elapsed := time.Since(started)
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}

	if resp.StatusCode != h.cfg.ExpectStatus {
		return fmt.Errorf("status %d, want %d", resp.StatusCode, h.cfg.ExpectStatus)
	}
	if budget := h.cfg.MaxLatency.Duration; budget > 0 && elapsed > budget {
		return fmt.Errorf("took %s, budget %s", elapsed.Round(time.Millisecond), budget)
	}
	if len(h.cfg.JSON) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	for _, a := range h.cfg.JSON {
		if err := checkAssertion(doc, a); err != nil {
			return err
		}
	}
	return nil
}

func checkAssertion(doc any, a config.Assertion) error {
	value, found := jsonpath.Lookup(doc, a.Path)
	if a.Exists != nil {
		if found != *a.Exists {
			if found {
				return fmt.Errorf("%s: should not exist", a.Path)
			}
			return fmt.Errorf("%s: missing", a.Path)
		}
		if !found {
			return nil
		}
	}
	if !found {
		return fmt.Errorf("%s: missing", a.Path)
	}
	if a.Equals != nil && !sameJSON(value, a.Equals) {
		return fmt.Errorf("%s: got %s, want %s", a.Path, show(value), show(a.Equals))
	}
	if a.MinItems != nil {
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: not a list", a.Path)
		}
		if len(list) < *a.MinItems {
			return fmt.Errorf("%s: %d items, want at least %d", a.Path, len(list), *a.MinItems)
		}
	}
	return nil
}

// sameJSON compares after a JSON round trip, so 3 (int) equals 3.0 (float64).
func sameJSON(a, b any) bool {
	var x, y any
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	_ = json.Unmarshal(ab, &x)
	_ = json.Unmarshal(bb, &y)
	return reflect.DeepEqual(x, y)
}

func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// BookingPurchase walks the path a real buyer takes, end to end:
// create an event, find its seat, buy it, read the order back, check the
// seat is gone, and (if other_key is set) check another buyer can't read it.
type BookingPurchase struct {
	cfg    config.Check
	client *http.Client
	now    func() time.Time
}

const journeyPrice = 4200

func (b *BookingPurchase) Run(ctx context.Context) error {
	var event struct {
		ID int `json:"id"`
	}
	if err := b.call(ctx, 1, "create a one-seat event", "POST", "/admin/events", b.cfg.AdminKey, map[string]any{
		"name":        "platform-probe journey",
		"venue":       "Synthetic monitoring",
		"starts_at":   b.now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
		"seats":       1,
		"price_cents": journeyPrice,
	}, http.StatusCreated, &event); err != nil {
		return err
	}

	var seats struct {
		Items []struct {
			ID int `json:"id"`
		} `json:"items"`
	}
	if err := b.call(ctx, 2, "list its seats", "GET", fmt.Sprintf("/events/%d/seats", event.ID), "", nil, http.StatusOK, &seats); err != nil {
		return err
	}
	if len(seats.Items) != 1 {
		return fmt.Errorf("step 2 (list its seats): %d seats available, want 1", len(seats.Items))
	}

	var order struct {
		ID         int `json:"id"`
		TotalCents int `json:"total_cents"`
	}
	if err := b.call(ctx, 3, "buy the seat", "POST", "/orders", b.cfg.BuyerKey, map[string]any{
		"seat_ids": []int{seats.Items[0].ID},
	}, http.StatusCreated, &order); err != nil {
		return err
	}
	if order.TotalCents != journeyPrice {
		return fmt.Errorf("step 3 (buy the seat): charged %d cents, want %d", order.TotalCents, journeyPrice)
	}

	orderPath := fmt.Sprintf("/orders/%d", order.ID)
	if err := b.call(ctx, 4, "read the order back", "GET", orderPath, b.cfg.BuyerKey, nil, http.StatusOK, nil); err != nil {
		return err
	}

	var after struct {
		Available int `json:"available_seats"`
	}
	if err := b.call(ctx, 5, "check the seat is gone", "GET", fmt.Sprintf("/events/%d", event.ID), "", nil, http.StatusOK, &after); err != nil {
		return err
	}
	if after.Available != 0 {
		return fmt.Errorf("step 5 (check the seat is gone): %d seats still available, want 0", after.Available)
	}

	if b.cfg.OtherKey != "" {
		if err := b.call(ctx, 6, "another buyer can't read the order", "GET", orderPath, b.cfg.OtherKey, nil, http.StatusNotFound, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *BookingPurchase) call(ctx context.Context, step int, what, method, path, key string, body any, want int, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("step %d (%s): %w", step, what, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("step %d (%s): %w", step, what, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("step %d (%s): reading body: %w", step, what, err)
	}
	if resp.StatusCode != want {
		return fmt.Errorf("step %d (%s): status %d, want %d", step, what, resp.StatusCode, want)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("step %d (%s): bad JSON: %w", step, what, err)
		}
	}
	return nil
}
