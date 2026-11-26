# platform-probe

[![CI](https://github.com/Kurtis-H/platform-probe/actions/workflows/ci.yml/badge.svg)](https://github.com/Kurtis-H/platform-probe/actions/workflows/ci.yml)
[![CodeQL](https://github.com/Kurtis-H/platform-probe/actions/workflows/codeql.yml/badge.svg)](https://github.com/Kurtis-H/platform-probe/actions/workflows/codeql.yml)

A synthetic monitor written in Go. It runs scripted checks against the booking platform ([api-performance-lab](https://github.com/Kurtis-H/api-performance-lab)'s API and [booking-web](https://github.com/Kurtis-H/booking-web)) at the same time, on a schedule, and exposes the results as Prometheus metrics for the existing Grafana dashboard.

**Standard library only:** no third-party Go modules. Config is JSON, and metrics are written in the Prometheus text format by hand.

```
$ platform-probe run -config examples/booking.json
CHECK                RESULT  TIME   TRIES  DETAIL
api health           pass    12ms   1
event list           pass    26ms   1
orders need a key    pass    6ms    1
page size is capped  pass    12ms   1
purchase journey     pass    110ms  1
web app is up        pass    2ms    1

6 of 6 checks passed
```

The same run against the API with its page-size limit switched off (`PERF_BUGS=no_page_limit`):

```
page size is capped  FAIL    22ms   1      status 200, want 422

5 of 6 checks passed          (exit code 1)
```

## Checks

**`http`:** one request, then:

* the status code (`expect_status`, default 200)
* a latency budget (`max_latency`)
* values in the JSON body (`json`), using dotted paths like `items.0.available_seats`:
  * `equals`: the value matches
  * `exists`: the path is there, or with `false`, isn't
  * `min_items`: a list has at least this many entries

**`booking_purchase`:** the path a real buyer takes, end to end, with a numbered step in every error message:

1. create a one-seat event (admin key)
2. find its seat
3. buy it, and check the charge is right
4. read the order back
5. check the seat is no longer for sale
6. check a **different** buyer gets a 404 for that order (an access-control check, run continuously)

The journey makes a real event and order every time it runs, so point it at a test environment, or give it a longer `interval` than the other checks.

Probes follow redirects only on the same host, because Go forwards custom headers like `X-API-Key` when it follows a redirect.

Every check has a `timeout` per attempt and up to 5 `retries` with backoff (200 ms, 400 ms, 800 ms...). All checks in a round run concurrently. Values like `${API_URL}` are filled from the environment, so one file works locally, in Docker and in CI.

```json
{
  "interval": "30s",
  "checks": [
    {
      "name": "event list",
      "type": "http",
      "url": "${API_URL}/events?limit=20",
      "max_latency": "1s",
      "retries": 1,
      "json": [{"path": "items", "min_items": 1}, {"path": "limit", "equals": 20}]
    },
    {
      "name": "purchase journey",
      "type": "booking_purchase",
      "base_url": "${API_URL}",
      "admin_key": "key-admin",
      "buyer_key": "key-user-11",
      "other_key": "key-user-12"
    }
  ]
}
```

Config mistakes fail loudly. Unknown fields such as a typo'd `expect_staus` are rejected, and every problem in a file is reported at once.

## Commands

```bash
platform-probe validate -config probes.json         # check the file
platform-probe run -config probes.json [-json]      # once; exit 1 if anything failed (good for CI)
platform-probe serve -config probes.json -listen :9100
```

`serve` runs a round every `interval` and serves:

| Path | What |
|---|---|
| `/metrics` | `probe_success`, `probe_duration_seconds`, `probe_attempts`, `probe_last_success_timestamp_seconds`, `probe_runs_total{result}`, `probe_rounds_total` |
| `/status` | the latest result of every check, as JSON |
| `/healthz` | liveness |

It stops cleanly on Ctrl-C or SIGTERM. `examples/prometheus.yml` has the scrape config to add to api-performance-lab's Prometheus.

## How it's tested

| | |
|---|---|
| Unit | Table-driven tests for every package: config parsing and validation, JSON paths, every assertion type, retries and backoff, per-attempt timeouts, metrics output and label escaping |
| Fakes | `httptest` servers, including a fake booking API that can overcharge, leave a sold seat on sale, or leak orders between buyers. The journey must catch each one |
| Concurrency | Every test runs with `-race`. One test proves checks run in parallel; another records and scrapes metrics from many goroutines at once |
| Fuzzing | `FuzzLookup` (JSON paths) and `FuzzParse` (config) run in CI on every change |
| Live | CI starts the real API, Postgres and web app, expects every probe to pass, then switches on a regression and expects exactly the right probe to fail |
| Static | `gofmt`, `go vet`, staticcheck, govulncheck, CodeQL |

Statement coverage is 89–100% per package, and CI fails below 85% overall.

## Run with Docker

```bash
docker build -t platform-probe .
docker run -p 9100:9100 -e API_URL=http://host.docker.internal:8000 -e WEB_URL=http://host.docker.internal:4173 platform-probe
```

The image is a static binary on distroless (no shell, runs as non-root) with `examples/booking.json` built in.

## Layout

```
cmd/platform-probe/   CLI: validate, run, serve
internal/config/      JSON config, defaults, validation
internal/checks/      http and booking_purchase checks
internal/jsonpath/    dotted-path lookup in decoded JSON
internal/runner/      concurrent rounds, timeouts, retries
internal/metrics/     latest results and Prometheus text output
examples/             booking platform probes and a Prometheus scrape config
```
