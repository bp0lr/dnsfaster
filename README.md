# dnsfaster

[![CI](https://github.com/bp0lr/dnsfaster/actions/workflows/ci.yml/badge.svg)](https://github.com/bp0lr/dnsfaster/actions/workflows/ci.yml)

Validate DNS resolver answers, measure latency and reliability, and export the best servers for your requirements. Written in Go, with concurrent checks, IPv6 support and structured output.

By default, dnsfaster first builds a trusted reference consensus, checks each candidate against that consensus and verifies negative answers across several domains. It then measures random-subdomain A queries, accepting `NXDOMAIN` as a successful measurement.

## Install

Requires **Go 1.27.1 or newer**:

```sh
go install github.com/bp0lr/dnsfaster@latest
```

Put your Go binary directory (`go env GOPATH`, followed by `bin`, unless you set `GOBIN`) in your `PATH`.

[Versioned releases](https://github.com/bp0lr/dnsfaster/releases) provide binaries and SHA-256 checksums for Linux, Windows and macOS, on amd64 and arm64. [CI artifacts](https://github.com/bp0lr/dnsfaster/actions/workflows/ci.yml) are also available for development commits.

Build from source:

```sh
git clone https://github.com/bp0lr/dnsfaster.git
cd dnsfaster
go build -trimpath -o dnsfaster .
```

On Windows, use `go build -trimpath -o dnsfaster.exe .`. `dnsfaster --version` prints the release version, installed module version or embedded Git revision to stdout. A modified source tree adds `+dirty`; `dev` is used only when build metadata is unavailable.

## Quick start

Create `dnslist.txt` with one resolver per line. This example assumes you operate a resolver listening locally:

```text
# Local resolver
127.0.0.1
```

Validate, measure and export:

```sh
dnsfaster --in dnslist.txt --out resolvers.txt
```

Select the best 20 passing resolvers by p95 latency:

```sh
dnsfaster --in dnslist.txt --out resolvers.txt --tests 100 --filter-rate 95 --filter-p95 400 --sort p95 --top 20
```

Use resolvers you operate or that permit this traffic. The default global limit of 50 queries per second covers reference checks, candidate validation, prechecks, measurements and TCP retries.

## Correctness validation

### Reference consensus (default)

The default reference resolvers are `1.1.1.1`, `8.8.8.8` and `9.9.9.9`. At least two must return the same complete A-record set for the positive domain. You can supply your own references:

```sh
dnsfaster --in dnslist.txt --domain stable.example --baseline 192.0.2.10,192.0.2.11,192.0.2.12 --out resolvers.txt
```

The documentation addresses above are placeholders. Replace them with your reference servers and a domain with stable answers. Reference queries run once per execution, and their immutable results are shared by all candidate workers.

- Positive validation checks the root domain and any additional `--positive-domain` values. Address order and TTL do not affect equality. CNAME chains are followed within the answer, and unrelated addresses are ignored. Empty, conflicting or mismatched answers fail.
- Negative validation requires `NXDOMAIN` without answer records for random names under the root domain and, by default, `facebook.com`, `paypal.com`, `google.com`, `bet365.com` and `wikileaks.com`.
- `--negative-domain` replaces the extra default domains; the root domain always remains included. Use `--negative-domain=` to check only the root domain.
- `--baseline-quorum` must be a strict majority of distinct configured reference endpoints. Duplicate endpoints do not add votes. Different hostnames may still refer to the same physical resolver; select independent references yourself.
- Failure to reach quorum stops the run before candidate checks and preserves existing output. A successful majority can tolerate a disagreeing or unavailable minority. As soon as every question reaches quorum, pending reference queries are canceled and their workers are joined before candidate checks start. An impossible quorum also stops early.

Domains with geographically varying or changing answer sets can fail this comparison even when the resolvers are working. Select a stable domain you control, or supply explicit expected answers.

### Explicit expected answers

This mode contacts candidate resolvers only. It does not query public reference resolvers:

```sh
dnsfaster --resolver 127.0.0.1:5353 --domain service.internal --validation expected --expect service.internal=192.0.2.10,192.0.2.11 --negative-domain= --out resolvers.txt
```

Configure your local DNS server with the corresponding records first. Every positive domain must have a nonempty, complete expected set for each selected record type. Use repeated `--positive-domain` and `--expect` flags for multiple domains. In baseline mode, explicit answers can also override the positive expectation for selected domains while the remaining reference checks continue.

### A and AAAA validation

`--record-types A` is the default. Use `--record-types AAAA` or `--record-types A,AAAA` to validate IPv6 answers, including CNAME chains. Positive and negative checks run independently for every selected type. A domain without addresses of a selected type fails validation; choose domains that publish those records.

```sh
dnsfaster --resolver 127.0.0.1:5353 --domain service.internal --validation expected --record-types A,AAAA --expect service.internal=192.0.2.10,2001:db8::10 --negative-domain=
```

`--expect` infers the record type from each address and normalizes IPv6 spelling. Each supplied family must be enabled by `--record-types`. In baseline mode, an explicit A set can override A while references determine AAAA, or vice versa. Resolver transport addresses and DNS record types are independent: an IPv4 resolver endpoint can answer AAAA queries. Prechecks and latency measurements remain A queries.

### Bounded validation retries

`--validation-retries 1` allows one extra attempt after a timeout or transport failure in reference and candidate correctness checks. Set it to `0` to disable retries, or up to `3`. Mismatched answers, missing records, invalid replies and DNS error codes such as SERVFAIL are not retried. Each attempt uses the shared QPS limiter and its own `--timeout`; `--max-duration` still bounds the whole run. With one retry, a failed logical check can consume two query timeouts plus rate-limit waits.

Prechecks and measurement queries do not use these retries. Their failures remain visible in the measurement results.

### Measurement-only compatibility mode

```sh
dnsfaster --in dnslist.txt --validation off --out resolvers.txt
```

This retains the previous NXDOMAIN measurement behavior and optional prechecks, skipping positive correctness checks and the additional negative-domain checks. It does not contact reference resolvers.

Validation establishes agreement for the sampled names and selected references. It does not guarantee DNSSEC validation, correctness for every domain, or future resolver behavior.

## Input and exclusions

IPv4, IPv6 and hostnames are supported. Port 53 is the default; use brackets for IPv6 with an explicit port:

```text
127.0.0.1
127.0.0.1:5353
::1
[::1]:5353
localhost:5353
```

Blank lines, surrounding whitespace, UTF-8 BOMs and `#` comments are ignored. Equivalent endpoints are deduplicated, including an explicit default port. Hostnames are normalized but are not deduplicated against their resolved IP addresses. Invalid entries fail with a line number before DNS checks start.

`--resolver` accepts individual endpoints and can be combined with `--in`. Input and exclusion lists accept local files or explicit HTTP(S) URLs:

```sh
dnsfaster --resolver 127.0.0.1:5353 --validation off
dnsfaster --in dnslist.txt --exclude 192.0.2.0/24 --exclude-file exclusions.txt --out resolvers.txt
```

Bare host/IP exclusions remove every port for that host. An explicit endpoint excludes only that port. CIDRs match literal IP inputs; hostname exclusions and CIDRs do not perform additional DNS resolution. Excluded entries are removed before candidate checks and do not remove reference servers from `--baseline`.

URL downloads have a 15-second timeout, a 32 MiB limit and at most five redirects. HTTP errors and invalid lists stop the run. No public candidate list is downloaded automatically.

Read from stdin and export JSON:

```sh
cat dnslist.txt | dnsfaster --in - --out - --format json --include-filtered --quiet
```

PowerShell:

```powershell
Get-Content dnslist.txt | dnsfaster --in - --out - --format json --include-filtered --quiet
```

## Options

| Option | Default | Description |
| --- | --- | --- |
| `--in` | Unset | Resolver file, HTTP(S) URL or `-` for stdin. Supply this or `--resolver`. |
| `--resolver` | Unset | Individual endpoints; repeatable or comma-separated. |
| `--exclude` | Unset | Host, endpoint or CIDR exclusions; repeatable or comma-separated. |
| `--exclude-file` | Unset | Exclusion file or HTTP(S) URL. |
| `--out` | Unset | Output file or `-` for stdout. Without it, show only console diagnostics. |
| `--domain` | `example.com` | Positive root domain and base for measured random queries. |
| `--validation` | `baseline` | Correctness mode: `baseline`, `expected` or `off`. |
| `--record-types` | `A` | Correctness types: `A`, `AAAA` or `A,AAAA`; prechecks and measurements remain A. |
| `--validation-retries` | `1` | Extra attempts after correctness timeout/transport failures, from 0 to 3. |
| `--baseline` | Three public references | Reference endpoints; repeatable or comma-separated. Replaces the defaults. |
| `--baseline-quorum` | Strict majority | Required identical reference responses; `0` calculates the majority. |
| `--positive-domain` | Unset | Additional positive domains; repeatable or comma-separated. |
| `--negative-domain` | Five extra domains | Replace default negative-check domains; root always included. |
| `--expect` | Unset | Expected addresses as `domain=IP,IP`; repeatable, with family inferred from each address. |
| `--query-prefix` | Unset | Optional DNS label prefix, at most 46 characters, before a random suffix. |
| `--workers` | `10` | Concurrent candidate checks, from 1 to 251. One resolver per worker. |
| `--tests` | `10` | Measured queries per resolver, from 1 to 5000. |
| `--timeout` | `2s` | Per-query timeout, including an optional TCP retry. |
| `--max-duration` | `0` | Overall timeout for source downloads and DNS work; zero disables it. |
| `--qps` | `50` | Positive global query rate limit, at most 1000000. |
| `--precheck-tests` | Adaptive | `0` with correctness validation, `3` with `--validation off`. Explicit values override this; maximum 1000. |
| `--precheck-errors` | `1` | Allowed precheck failures. At least one must succeed when enabled. |
| `--filter-time` | `0` | Maximum average measurement latency in milliseconds; zero disables it. |
| `--filter-p95` | `0` | Maximum p95 measurement latency in milliseconds; zero disables it. |
| `--filter-errors` | `0` | Maximum measurement failures; zero disables it. |
| `--filter-rate` | `0` | Minimum success percentage; zero disables it. |
| `--tcp-fallback` | `false` | Retry truncated UDP replies over TCP. |
| `--sort` | `latency` | Sort by `latency`, `p95`, `rate` or `input`. |
| `--top` | `0` | Maximum passing exports after sorting; zero means all. All candidates are still measured. |
| `--format` | `dns` | Export `dns`, headered `csv` or `json`. |
| `--include-filtered` | `false` | Include rejected records in CSV/JSON, independently of `--top`. |
| `--progress` | `true` | Show startup and periodic candidate progress on stderr. |
| `--verbose` | `false` | Report each completed resolver on stderr. |
| `--quiet` | `false` | Suppress progress, completion messages, table and summary; errors remain visible. |
| `--save-dns` | `true` | Legacy selector; false writes headerless five-column CSV. Cannot accompany `--format`. |
| `--version` | | Print build version to stdout. |
| `--help` | | Show CLI help. |

Every enabled filter must pass. Threshold equality is accepted. Use `--filter-rate 100` to require zero measurement failures. Correctness checks are strict and are not relaxed by measurement filters or precheck tolerance.

## Results and measurement semantics

The console report and final summary use **stderr**. With `--out -`, stdout contains only the export. Passing records come first when sorting by a metric. Rate sorts descending, latency/p95 ascending, with average latency and input order used as tie breakers as applicable. `--sort input` preserves input order. `--top` limits passing exports after that ordering; it does not change the console table or promote rejected resolvers.

CSV columns:

```text
resolver,average_ms,success_percent,successes,failures,p50_ms,p95_ms,precheck_failures,filtered,reasons,errors,validation_checks,validation_failures,validation_retries
```

JSON uses the same names, with arrays for `reasons` and an object for `errors`. The CSV `errors` cell is a JSON object. CSV prints three decimal places; JSON retains computed precision. An empty JSON export is `[]`. The legacy CSV format remains:

```text
resolver,average_ms,success_percent,successes,failures
```

After correctness validation, optional prechecks require a nontruncated `NOERROR` reply from the root domain. Measurement queries then require nontruncated `NXDOMAIN` replies with no answer records. Each resolver receives the same generated measurement names. Mean, p50 and p95 include successful measurement queries only; correctness and precheck timings do not enter those statistics.

Percentiles use nearest rank. With ten successful samples, p95 equals the maximum; use more samples when ranking tail latency. No successful measurements always means rejection. Latency fields are zero in structured exports when there are no successful samples; the console displays `n/a`.

A validation failure rejects the resolver immediately and skips its prechecks and measurements. Validation checks/failures and precheck failures have separate counters. `validation_checks` counts logical candidate checks, `validation_failures` counts terminal failures, and `validation_retries` counts extra candidate attempts, including recovered failures. Reference retries are shared setup work and do not enter per-candidate counters. Reasons identify positive mismatches, missing answers, negative-check failures, timeouts, transport errors and unexpected DNS codes.

TCP fallback shares the UDP query's timeout budget and the global limiter. Waiting for the initial rate-limit slot is excluded from latency. Connection setup and a TCP retry, including its rate-limit wait, are included. Every query opens a fresh connection. Caches, wildcard DNS and network conditions affect results; random labels do not guarantee every upstream query bypasses caching.

File exports are written to a temporary file in the destination directory and replace the previous file only after a complete successful write. Input and exclusion files cannot be overwritten by the output. Cancellation, reference-quorum failure and write errors preserve the previous destination. Replacement uses OS rename semantics and is not guaranteed atomic on every filesystem. Ctrl+C stops queued DNS work and interrupts active connections.

| Exit code | Meaning |
| --- | --- |
| `0` | At least one resolver passed, or help/version was requested. |
| `1` | Input/output or runtime failure, reference-quorum failure, deadline expiry, or no passing resolvers. A completed run with no passing resolvers still writes its requested export. |
| `2` | Invalid arguments. |
| `130` | Interrupted with Ctrl+C. |

## Functional baseline

[dnsvalidator](https://github.com/vortexau/dnsvalidator) is the functional reference for resolver correctness checks. dnsfaster implements its documented core capabilities through an independent Go implementation:

| Capability | dnsfaster |
| --- | --- |
| Single resolver, list or stdin | `--resolver`, `--in`, `--in -` |
| Lists and exclusions from files or URLs | `--in`, `--exclude`, `--exclude-file`; CIDRs also supported |
| Positive answers against trusted references | Complete relevant A/AAAA sets, configurable majority and explicit expected-answer mode |
| Negative checks across multiple domains | Root plus five default domains, configurable replacements and query prefix |
| Concurrent checks and time limits | Worker pool, per-query and overall timeout, shared rate limit |
| Quiet and verbose diagnostics | `--quiet`, `--verbose`, clean stdout exports; output is always uncolored |
| Filtered resolver output | DNS list, CSV/JSON, ranking, top N, mean and p95 filters |

CLI flag spelling is not intended to be a drop-in replacement. Candidates must be explicitly supplied. The focus remains correctness, predictable concurrency and useful measurements; implementation language alone does not establish a speed advantage.

## Performance and compatibility

Default A validation now sends **17 queries per healthy candidate**: one positive check, six negative checks and ten measurements. This is 15% fewer than the previous 20-query default because strict validation makes the three extra prechecks redundant. Measurement-only mode retains three prechecks and ten measurements. Explicit `--precheck-tests` values always win.

With three healthy default references and no retries, shared setup sends 14 to 21 queries, depending on scheduling and early cancellation after quorum. A silent minority no longer forces the majority to wait for its timeout. Selecting both A and AAAA increases candidate correctness checks from 7 to 14, for 24 total queries with the default ten A measurements. Additional domains, retries and TCP fallback can increase these counts.

### Controlled comparison with dnsvalidator

The reproducible harness is [benchmarks/compare.py](benchmarks/compare.py). It uses the unmodified [dnsvalidator revision `146c9b0`](https://github.com/vortexau/dnsvalidator/tree/146c9b0e24d806b25697fbb541bf9f19a3086d41), with a runtime adapter that maps reference and candidate addresses to ephemeral loopback ports and sets dnspython's query timeout/lifetime to 200 ms. The adapter blocks outbound traffic outside loopback. It does not change upstream acceptance logic.

Conditions: September 7, 2026, Windows amd64, Ryzen 9 3900X, Go 1.27.1, Python 3.12.3, dnspython 2.8.0, psutil 7.2.2, requests 2.34.2 and colorclass 2.2.2. Each case has one excluded warmup followed by five measured runs, alternating program order. Both use ten workers and the same local UDP fixture. dnsfaster uses a QPS limit of 1000000 so the default production rate limit does not dominate this throughput experiment.

| Workload | Tool | Median seconds | Median peak MiB |
| --- | --- | --- | --- |
| equivalent | dnsfaster | 0.108 | 13.4 |
| equivalent | dnsvalidator | 0.904 | 45.8 |
| full | dnsfaster | 0.292 | 14.9 |
| full | dnsvalidator | 0.964 | 45.8 |
| correctness | dnsfaster | 0.644 | 11.0 |
| correctness | dnsvalidator | 2.756 | 45.6 |

Time includes process startup, Python imports, execution and exit. Memory is the median OS peak working set of each child process, sampled every 2 ms; the shared fixture and runner are excluded. These are local end-to-end tool measurements, not network latency results or a guarantee of the same ratios on other systems. The Python fixture, interpreter startup and Windows scheduling affect the results.

- **Equivalent workload:** 100 healthy candidates, six A/NXDOMAIN queries each under the same root, zero reference queries. dnsfaster uses `--validation off --precheck-tests 0 --tests 6 --filter-rate 100`; the adapter invokes upstream's candidate function with preloaded reference state and five repeated root checks. The harness asserts exactly 600 candidate queries for each tool. Both accept all 100 candidates. Random label generation remains specific to each tool.
- **Full validation:** 100 healthy candidates, shared setup included, two local references for both tools. dnsfaster sends 1700 candidate queries plus 14 reference queries; upstream sends 600 plus 8. Both accept all candidates. The different work means this row cannot isolate algorithmic speed.
- **Correctness:** 30 candidates, six each that are healthy, return an incorrect positive A set, substitute answers for nonexistent names, stay silent, or drop the first packet and then answer correctly. The expected accepted set contains the 12 healthy or recovering candidates. dnsfaster has zero false accepts and zero false rejects; this upstream revision has six false accepts and zero false rejects in every repetition. The affected cases are the incorrect positive A sets. This finite synthetic sample is not a general accuracy estimate. AAAA is covered separately by dnsfaster integration tests because the upstream check uses A.

To reproduce, use Python 3.12 with the dependency versions listed above, then run from the repository root:

```sh
git clone https://github.com/vortexau/dnsvalidator.git dist/dnsvalidator-upstream
git -C dist/dnsvalidator-upstream checkout 146c9b0e24d806b25697fbb541bf9f19a3086d41
go build -trimpath -o dist/dnsfaster-benchmark .
python benchmarks/compare.py --binary dist/dnsfaster-benchmark --upstream dist/dnsvalidator-upstream
```

On Windows, add `.exe` to the build output and `--binary` argument. The harness writes individual process outputs and raw measurements, binary SHA-256 and build metadata to ignored `dist/comparison/`, including `results.json`. It does not download resolver lists or run queries against public resolvers. Benchmark dependencies are developer tools only; the Go binary does not depend on Python.

Existing flags remain available. Use `--validation off` for measurement-only behavior, and specify `--precheck-tests 3` to retain extra probes alongside correctness validation. Legacy CSV remains five columns; headered CSV appends the retry counter after existing fields.

## Development

All integration tests use local DNS and HTTP servers. They never query public resolvers or download public candidate lists.

```sh
go mod verify
go test -count=1 -timeout=60s ./...
go vet ./...
go test -race ./...
go test -run '^$' -bench . -benchmem ./...
```

The race detector requires a supported platform and a C compiler. CI tests Windows, Linux and macOS, runs the race detector and dependency vulnerability check on Linux, and builds six platform binaries. Benchmarks cover resolver parsing, the measurement path, correctness checks and different precheck counts. Keep the same toolchain, machine and test conditions for comparisons.

To reproduce a release binary, check out its tag, set `CGO_ENABLED=0`, `GOOS` and `GOARCH`, and build with `-trimpath -ldflags "-s -w -X main.version=TAG"`. Replace `TAG` with the actual release tag.

## Credits

This project was adapted from [Jules Rigaudie's dnsfaster](https://gitlab.com/jules.rigaudie/dnsfaster). The original project moved to GitLab, which is why this repository is not a GitHub fork. Thanks to Jules for the original work.
