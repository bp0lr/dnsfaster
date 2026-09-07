# dnsfaster

[![CI](https://github.com/bp0lr/dnsfaster/actions/workflows/ci.yml/badge.svg)](https://github.com/bp0lr/dnsfaster/actions/workflows/ci.yml)

Measure DNS resolver latency and reliability, rank the results, and export the servers that meet your thresholds.

dnsfaster sends A queries for random subdomains of a chosen domain. A successful measurement is an `NXDOMAIN` response, indicating that the queried name does not exist. This checks a specific resolver behavior, not general DNS correctness or browsing speed.

## Install

Requires **Go 1.27.1 or newer**. Install [Go](https://go.dev/dl/), then run:

```sh
go install github.com/bp0lr/dnsfaster@latest
```

Put your Go binary directory (`go env GOPATH`, followed by `bin`, unless you set `GOBIN`) in your `PATH`.

Build from source:

```sh
git clone https://github.com/bp0lr/dnsfaster.git
cd dnsfaster
go build -trimpath -o dnsfaster .
```

On Windows, use `go build -trimpath -o dnsfaster.exe .`.

[CI runs](https://github.com/bp0lr/dnsfaster/actions/workflows/ci.yml) also provide binaries and SHA-256 checksums for Linux, Windows and macOS, on amd64 and arm64. Download the artifact matching your system from a successful run. GitHub requires sign-in to download workflow artifacts.

## Quick start

Create `dnslist.txt` with one resolver per line. The example assumes you have a DNS resolver listening locally:

```text
# Local resolver
127.0.0.1
```

Run a small check:

```sh
dnsfaster --in dnslist.txt --out resolvers.txt
```

Filter by latency and reliability:

```sh
dnsfaster --in dnslist.txt --out resolvers.txt --domain example.com --tests 100 --filter-time 400 --filter-errors 10 --filter-rate 90
```

Use resolvers you operate or that permit this traffic. The default global limit is 50 queries per second, shared by every worker, including prechecks and TCP retries.

## Input

IPv4, IPv6 and hostnames are supported. Port 53 is the default; use brackets for IPv6 with an explicit port:

```text
127.0.0.1
127.0.0.1:5353
::1
[::1]:5353
localhost:5353
```

Blank lines, surrounding whitespace, UTF-8 BOMs and `#` comments are ignored. Equivalent addresses are deduplicated, including an explicit default port. Hostnames are normalized but are not deduplicated against their resolved IP addresses. Invalid entries fail with a line number before any DNS checks start.

Read from stdin and export a machine-readable report:

```sh
cat dnslist.txt | dnsfaster --in - --out - --format json --include-filtered --quiet
```

PowerShell equivalent:

```powershell
Get-Content dnslist.txt | dnsfaster --in - --out - --format json --include-filtered --quiet
```

## Options

| Option | Default | Description |
| --- | --- | --- |
| `--in` | Required | Resolver file, or `-` for stdin. |
| `--out` | Unset | Output file, or `-` for stdout. Without it, only the console report is shown. |
| `--domain` | `example.com` | Base domain without wildcard DNS. |
| `--workers` | `10` | Concurrent resolver checks, from 1 to 251. Each worker measures one resolver at a time. |
| `--tests` | `10` | Measurements per resolver, from 1 to 5000. |
| `--timeout` | `2s` | Query timeout, including an optional TCP retry. Accepts Go durations such as `500ms`. |
| `--qps` | `50` | Positive global query rate limit, at most 1000000. |
| `--precheck-tests` | `3` | Queries for the base domain before measuring; `0` disables prechecks, maximum 1000. |
| `--precheck-errors` | `1` | Allowed precheck failures. At least one precheck must succeed when enabled. |
| `--filter-time` | `0` | Reject mean latency above this many milliseconds. Zero disables the filter. |
| `--filter-errors` | `0` | Reject measurement failure counts above this threshold. Zero disables the filter. |
| `--filter-rate` | `0` | Reject success percentages below this threshold. Zero disables the filter. |
| `--tcp-fallback` | `false` | Retry truncated UDP responses over TCP. |
| `--sort` | `latency` | Rank by `latency`, `rate`, or preserve `input` order. |
| `--format` | `dns` | Export `dns`, `csv` with a header, or `json`. |
| `--include-filtered` | `false` | Include rejected resolvers in CSV or JSON exports. |
| `--quiet` | `false` | Suppress the console report; errors still go to stderr. |
| `--save-dns` | `true` | Legacy selector. `--save-dns=false` writes five-column CSV without a header. Cannot be combined with `--format`. |
| `--version` | | Print the build version; source builds default to `dev`. |
| `--help` | | Show CLI help. |

Every enabled filter must pass. Threshold equality is accepted. To require zero measurement failures, use `--filter-rate 100`.

## Output

The console report goes to **stderr**. When `--out -` is selected, stdout contains only the requested export. File exports are prepared in the destination directory and replace the previous file only after a complete successful write. Input and output cannot refer to the same file. Cancellation leaves existing output files intact. Replacement uses the operating system's rename semantics, which are not guaranteed atomic on every platform or filesystem.

DNS-list output contains passing resolvers only, one per line. Results default to ascending mean latency, with passing resolvers ahead of filtered results. `--sort rate` uses descending success rate and then ascending latency. Resolvers without successful measurements sort after measured resolvers within their group.

```sh
dnsfaster --in dnslist.txt --out report.csv --format csv --include-filtered
dnsfaster --in dnslist.txt --out report.json --format json --include-filtered --sort rate
```

CSV columns:

```text
resolver,average_ms,success_percent,successes,failures,p50_ms,p95_ms,precheck_failures,filtered,reasons,errors
```

The `errors` cell is a JSON object containing counts by reason. JSON exports use the same field names, with arrays for `reasons` and an object for `errors`. CSV prints three decimal places; JSON retains the computed precision. Latency fields are zero when there are no successful measurements; the console shows `n/a` in that case. An empty JSON export is `[]`.

The legacy `--save-dns=false` format retains these five columns without a header:

```text
resolver,average_ms,success_percent,successes,failures
```

## How measurements work

1. Normalize and deduplicate the resolver list.
2. Run up to three prechecks per resolver against the configured base domain. A nontruncated `NOERROR` response passes. Stop early when failures exceed the allowed count.
3. Send A queries for random nonexistent subdomains. Every resolver receives the same generated names. Only a nontruncated `NXDOMAIN` response counts as a successful measurement.
4. Calculate mean, p50 and p95 from successful measurements, apply filters and sort the results. Percentiles use the nearest-rank method.

No successful measurements always means rejection, even with all filters disabled. Precheck failures are reported separately from measurement failures. A failed precheck skips that resolver's measurements and records `precheck_failed`.

Errors distinguish timeouts, transport errors, truncated replies and unexpected DNS response codes. Optional TCP fallback shares the UDP query's timeout budget and the global rate limit. Time waiting for the initial rate-limit slot is excluded from latency; connection setup and any TCP retry, including its rate-limit wait, are included. Each query opens a fresh connection.

Wildcard DNS, caches, network conditions and the selected domain affect results. Compare runs under similar conditions. The base domain should exist and return `NXDOMAIN` for random nonexistent names. Random labels do not guarantee that every upstream query bypasses caching.

Ctrl+C cancels queued work and interrupts active DNS connections. Exit codes:

| Code | Meaning |
| --- | --- |
| `0` | At least one resolver passed, or help/version was requested. |
| `1` | Input/output failure, another runtime error, or no resolver passed. A completed run with no passing resolvers still writes its requested export. |
| `2` | Invalid CLI arguments. |
| `130` | Interrupted with Ctrl+C. |

## Changes from the original version

- Go 1.27.1 and updated dependencies.
- Workers keep their own state, and the collector waits for all workers before completing.
- Default prechecks reduced from 20 to 3, with one tolerated failure. A healthy resolver now receives 13 queries instead of 30 with default settings, a 56.7% reduction in query count. This is not a runtime speedup claim.
- A global rate limit of 50 queries per second replaces unrestricted scheduling. Increase it only within the resolver's allowed limits.
- Input validation, deduplication, IPv6, custom ports and stdin support.
- Precise latency statistics, sorting, structured exports and explicit failure reasons.
- No-success resolvers are always rejected. CLI/runtime errors now return nonzero status codes.
- Console output moved to stderr for clean pipelines. Existing flags remain available; the legacy CSV retains its column order and now prints decimal measurements.

## Development

All integration tests use local DNS servers on ephemeral ports. They do not query public resolvers.

```sh
go mod verify
go test -count=1 -timeout=60s ./...
go vet ./...
go test -race ./...
go test -run '^$' -bench . -benchmem ./...
```

The race detector requires a supported platform and a C compiler. CI runs tests on Windows, Linux and macOS, and the race detector on Linux. Benchmarks cover parsing 1000 resolver entries, four local DNS measurements, and the cost of different precheck counts. See [performance measurements](PERFORMANCE.md) for results and limitations.

## Credits

This project was adapted from [Jules Rigaudie's dnsfaster](https://gitlab.com/jules.rigaudie/dnsfaster). The original project moved to GitLab, which is why this repository is not a GitHub fork. Thanks to Jules for the original work.
