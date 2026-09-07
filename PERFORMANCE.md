# Performance measurements

Measured on September 7, 2026 with Go 1.27.1, Windows amd64, and an AMD Ryzen 9 3900X. All DNS traffic stayed on loopback, using an ephemeral port.

## Precheck cost

`BenchmarkPrecheckBudget` compares the old precheck count of 20 with the new default of 3. Both cases run the same new implementation, the same local DNS server, and 10 successful measurements per resolver. Rate limiting is disabled inside this benchmark to isolate exchange overhead.

```sh
go test -run '^$' -bench BenchmarkPrecheckBudget -benchmem -benchtime=1s ./...
```

| Prechecks | Queries per resolver | Time per resolver | Bytes allocated | Allocations |
| --- | --- | --- | --- | --- |
| 20 | 30 | 10.087 ms | 106839 | 1463 |
| 3 | 13 | 4.148 ms | 46672 | 646 |

The configured query count falls by 56.7%. This local sample took about 59% less time with three prechecks. It is a comparison of precheck settings, not a benchmark against the original binary and not a predicted speedup for public resolvers. Network latency, timeout rates, the global rate limit and caching will change runtime.

The precheck now stops when the allowed failure count is exceeded. For example, with 20 prechecks and one allowed failure, a resolver returning only `SERVFAIL` receives two queries instead of twenty. An integration test verifies this count.

## Other improvements

- Equivalent input addresses are deduplicated before scheduling.
- Each worker owns its query clients and reuses its latency sample buffer across resolvers.
- Only one result per resolver crosses the result channel. The collector waits for all workers before returning.
- Workers are limited by the resolver count. Memory for latency samples is bounded by active workers times measurements per resolver.
- Cancellation closes active DNS connections and stops queued work.

`BenchmarkReadResolvers` and `BenchmarkLocalResolver` provide baselines for future parser and query changes. Run all benchmarks with:

```sh
go test -run '^$' -bench . -benchmem ./...
```

For comparisons across commits, use the same Go version, machine, benchmark duration and test environment. Collect repeated samples before drawing conclusions from small timing differences.
