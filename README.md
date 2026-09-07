# dnsfaster

Check DNS resolvers for response time and reliability, then export the servers that meet your thresholds.

dnsfaster sends A queries for random subdomains of a test domain. A successful measurement is an `NXDOMAIN` response, indicating that the queried name does not exist. It measures this specific behavior, not general DNS correctness or browsing speed.

## Install

Install [Go](https://go.dev/dl/), then run:

```sh
go install github.com/bp0lr/dnsfaster@latest
```

Make sure your Go binary directory (`go env GOPATH`, followed by `bin`) is in your `PATH`.

To build from source:

```sh
git clone https://github.com/bp0lr/dnsfaster.git
cd dnsfaster
go build -o dnsfaster .
```

On Windows, use `go build -o dnsfaster.exe .`.

## Quick start

Create `dnslist.txt` with one resolver IPv4 address per line. Use resolvers you operate or that permit this traffic.

```text
127.0.0.1
```

Run a small check against your local DNS resolver:

```sh
dnsfaster --in dnslist.txt --out resolvers.txt --tests 10
```

Filter by latency and reliability:

```sh
dnsfaster --in dnslist.txt --out resolvers.txt --domain example.com --tests 100 --workers 10 --filter-time 400 --filter-errors 10 --filter-rate 90
```

The output file contains only servers that pass every enabled filter. The console also shows filtered results.

## Options

| Option | Default | Description |
| --- | --- | --- |
| `--in` | Required | File containing one resolver per line. |
| `--out` | Unset | Save passing resolvers to this file. |
| `--domain` | `example.com` | Base domain for random subdomain queries. |
| `--workers` | `10` | Concurrent workers, from 1 to 251. |
| `--tests` | `10` | Measurements per resolver, from 1 to 5000. |
| `--filter-time` | `0` | Reject average latency above this many milliseconds. Zero disables the filter. |
| `--filter-errors` | `0` | Reject failure counts above this threshold. Zero disables the filter. |
| `--filter-rate` | `0` | Reject success percentages below this threshold. Zero disables the filter. |
| `--save-dns` | `true` | Write only resolver addresses. Use `--save-dns=false` for CSV rows. |

CSV output currently has no header and uses these columns:

```text
resolver,average_ms,success_percent,successes,failures
```

## Measurement notes

- Before measurements, each resolver receives 20 additional A queries for `example.com`. Any exchange error or truncated response excludes it from the measurement stage.
- The test domain must return `NXDOMAIN` for random nonexistent subdomains. Wildcard DNS can make a working resolver fail this test.
- Reported latency averages successful measurements only. Current output uses whole milliseconds.
- DNS caching, network conditions and the chosen domain affect results. Compare runs under similar conditions.
- The current implementation expects IPv4 addresses and uses port 53 over UDP.

## Development

```sh
go test ./...
go vet ./...
```

## Credits

This project was adapted from [Jules Rigaudie's dnsfaster](https://gitlab.com/jules.rigaudie/dnsfaster). The original project moved to GitLab, which is why this repository is not a GitHub fork. Thanks to Jules for the original work.
