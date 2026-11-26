package checks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kurtis-H/platform-probe/internal/config"
)

func boolp(b bool) *bool { return &b }
func intp(i int) *int    { return &i }

func httpCheck(url string, mutate func(*config.Check)) config.Check {
	c := config.Check{Name: "t", Type: config.TypeHTTP, Method: "GET", URL: url, ExpectStatus: 200}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func run(t *testing.T, c config.Check) error {
	t.Helper()
	checker, err := New(c, &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return checker.Run(context.Background())
}

func jsonServer(t *testing.T, status int, body string, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const page = `{"items": [{"id": 1}, {"id": 2}], "total": 30, "limit": 20}`

func TestHTTPCheck(t *testing.T) {
	ok := jsonServer(t, 200, page, 0)
	cases := []struct {
		name    string
		url     string
		mutate  func(*config.Check)
		wantErr string // "" means pass
	}{
		{"status matches", ok.URL, nil, ""},
		{"wrong status", jsonServer(t, 500, `{}`, 0).URL, nil, "status 500, want 200"},
		{"expected error status", jsonServer(t, 422, `{}`, 0).URL, func(c *config.Check) { c.ExpectStatus = 422 }, ""},
		{"equals passes", ok.URL, func(c *config.Check) {
			c.JSON = []config.Assertion{{Path: "total", Equals: 30}, {Path: "items.1.id", Equals: 2.0}}
		}, ""},
		{"equals fails", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "limit", Equals: 100}} }, "limit: got 20, want 100"},
		{"exists passes", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "items", Exists: boolp(true)}} }, ""},
		{"must not exist", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "total", Exists: boolp(false)}} }, "should not exist"},
		{"absent as expected", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "error", Exists: boolp(false)}} }, ""},
		{"missing path", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "nope", Equals: 1}} }, "nope: missing"},
		{"min items passes", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "items", MinItems: intp(2)}} }, ""},
		{"min items fails", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "items", MinItems: intp(3)}} }, "2 items, want at least 3"},
		{"min items on a number", ok.URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "total", MinItems: intp(1)}} }, "not a list"},
		{"not JSON", jsonServer(t, 200, `<html>`, 0).URL, func(c *config.Check) { c.JSON = []config.Assertion{{Path: "a", Exists: boolp(true)}} }, "not JSON"},
		{"over latency budget", jsonServer(t, 200, `{}`, 120*time.Millisecond).URL, func(c *config.Check) { c.MaxLatency.Duration = 50 * time.Millisecond }, "budget 50ms"},
		{"within latency budget", ok.URL, func(c *config.Check) { c.MaxLatency.Duration = time.Second }, ""},
		{"connection refused", "http://127.0.0.1:1", nil, "request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t, httpCheck(tc.url, tc.mutate))
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestHTTPCheckSendsMethodAndHeaders(t *testing.T) {
	var gotMethod, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotKey = r.Method, r.Header.Get("X-API-Key")
	}))
	defer srv.Close()
	err := run(t, httpCheck(srv.URL, func(c *config.Check) {
		c.Method = "HEAD"
		c.Headers = map[string]string{"X-API-Key": "key-user-1"}
	}))
	if err != nil || gotMethod != "HEAD" || gotKey != "key-user-1" {
		t.Fatalf("err %v method %q key %q", err, gotMethod, gotKey)
	}
}

func TestHTTPCheckHonoursContext(t *testing.T) {
	srv := jsonServer(t, 200, `{}`, 300*time.Millisecond)
	checker, _ := New(httpCheck(srv.URL, nil), http.DefaultClient)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := checker.Run(ctx); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("err = %v, want a deadline error", err)
	}
}

func TestNewRejectsUnknownType(t *testing.T) {
	if _, err := New(config.Check{Type: "ping"}, http.DefaultClient); err == nil {
		t.Fatal("expected an error")
	}
}

// fakeBooking is a tiny booking API with switchable bugs.
type fakeBooking struct {
	mu        sync.Mutex
	sold      bool
	chargeOff int  // added to the charged price
	leakOrder bool // any key can read any order
	keepSeat  bool // the seat stays listed after purchase
	failBuy   int  // status to return when buying
}

func (f *fakeBooking) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("POST /admin/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "admin" {
			reply(w, 401, map[string]string{"detail": "no"})
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["seats"] != 1.0 || in["price_cents"] != float64(journeyPrice) {
			t.Errorf("unexpected event body %v", in)
		}
		reply(w, 201, map[string]int{"id": 7, "seats": 1})
	})
	mux.HandleFunc("GET /events/7/seats", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		items := []map[string]int{}
		if !f.sold || f.keepSeat {
			items = append(items, map[string]int{"id": 70})
		}
		reply(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /events/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		available := 1
		if f.sold && !f.keepSeat {
			available = 0
		}
		reply(w, 200, map[string]int{"id": 7, "available_seats": available})
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		if f.failBuy != 0 {
			reply(w, f.failBuy, map[string]string{"detail": "nope"})
			return
		}
		f.mu.Lock()
		f.sold = true
		f.mu.Unlock()
		reply(w, 201, map[string]int{"id": 9, "total_cents": journeyPrice + f.chargeOff})
	})
	mux.HandleFunc("GET /orders/9", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") == "buyer" || f.leakOrder {
			reply(w, 200, map[string]int{"id": 9})
			return
		}
		reply(w, 404, map[string]string{"detail": "Order not found"})
	})
	return mux
}

func journey(t *testing.T, fake *fakeBooking, otherKey string) error {
	t.Helper()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	return run(t, config.Check{
		Name: "j", Type: config.TypeBookingPurchase, BaseURL: srv.URL,
		AdminKey: "admin", BuyerKey: "buyer", OtherKey: otherKey,
	})
}

func TestBookingPurchase(t *testing.T) {
	cases := []struct {
		name    string
		fake    *fakeBooking
		other   string
		wantErr string
	}{
		{"healthy", &fakeBooking{}, "someone-else", ""},
		{"healthy without the access check", &fakeBooking{}, "", ""},
		{"overcharges", &fakeBooking{chargeOff: 100}, "", "step 3 (buy the seat): charged 4300 cents, want 4200"},
		{"seat still for sale after purchase", &fakeBooking{keepSeat: true}, "", "step 5 (check the seat is gone): 1 seats still available"},
		{"another buyer can read the order", &fakeBooking{leakOrder: true}, "someone-else", "step 6 (another buyer can't read the order): status 200, want 404"},
		{"purchase refused", &fakeBooking{failBuy: 409}, "", "step 3 (buy the seat): status 409, want 201"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := journey(t, tc.fake, tc.other)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestBookingPurchaseWrongAdminKey(t *testing.T) {
	srv := httptest.NewServer((&fakeBooking{}).handler(t))
	defer srv.Close()
	err := run(t, config.Check{Name: "j", Type: config.TypeBookingPurchase, BaseURL: srv.URL, AdminKey: "nope", BuyerKey: "buyer"})
	if err == nil || !strings.Contains(err.Error(), "step 1 (create a one-seat event): status 401") {
		t.Fatalf("err = %v", err)
	}
}

func TestBookingPurchaseUnreachable(t *testing.T) {
	err := run(t, config.Check{Name: "j", Type: config.TypeBookingPurchase, BaseURL: "http://127.0.0.1:1", AdminKey: "a", BuyerKey: "b"})
	if err == nil || !strings.HasPrefix(err.Error(), "step 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewClientKeepsKeysOnTheSameHost(t *testing.T) {
	var sawKey atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey.Store(r.Header.Get("X-API-Key"))
	}))
	defer other.Close()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, srv.URL+"/ok", http.StatusFound)
		case "/elsewhere":
			// 127.0.0.1 vs localhost would share a host; a different port does not.
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	client := NewClient(time.Minute)
	withKey := func(path string) config.Check {
		return httpCheck(srv.URL+path, func(c *config.Check) { c.Headers = map[string]string{"X-API-Key": "key-admin"} })
	}

	ch, _ := New(withKey("/same"), client)
	if err := ch.Run(context.Background()); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}

	ch, _ = New(withKey("/elsewhere"), client)
	err := ch.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect") {
		t.Fatalf("cross-host redirect: got %v, want a refusal", err)
	}
	if k, _ := sawKey.Load().(string); k != "" {
		t.Fatalf("the other host received the key %q", k)
	}
}

func TestNewClientStopsRedirectLoops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	ch, _ := New(httpCheck(srv.URL+"/loop", nil), NewClient(time.Minute))
	if err := ch.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("got %v, want the loop to stop", err)
	}
}
