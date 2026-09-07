package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/time/rate"
)

func localDNS(t testing.TB, handler dns.HandlerFunc, tcp bool) endpoint {
	t.Helper()
	var packet net.PacketConn
	var listener net.Listener
	var err error
	if tcp {
		// Let TCP choose a permitted port first. On Windows, a UDP ephemeral
		// port can belong to an excluded TCP range.
		for range 10 {
			listener, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			packet, err = net.ListenPacket("udp", listener.Addr().String())
			if err == nil {
				break
			}
			_ = listener.Close()
		}
	} else {
		packet, err = net.ListenPacket("udp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	server := &dns.Server{PacketConn: packet, Handler: handler, NotifyStartedFunc: func() { close(ready) }}
	go func() {
		if err := server.ActivateAndServe(); err != nil {
			t.Errorf("serve UDP: %v", err)
		}
	}()
	<-ready
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	if tcp {
		started := make(chan struct{})
		stream := &dns.Server{Listener: listener, Handler: handler, NotifyStartedFunc: func() { close(started) }}
		go func() {
			if err := stream.ActivateAndServe(); err != nil {
				t.Errorf("serve TCP: %v", err)
			}
		}()
		<-started
		t.Cleanup(func() {
			if err := stream.Shutdown(); err != nil {
				t.Error(err)
			}
		})
	}
	ep, err := parseEndpoint(packet.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func replyWith(w dns.ResponseWriter, m *dns.Msg, rcode int) {
	r := new(dns.Msg)
	r.SetReply(m)
	r.Rcode = rcode
	_ = w.WriteMsg(r)
}

func testConfig(t testing.TB) config {
	t.Helper()
	c, err := parseConfig([]string{"--validation", "off", "--in", "-", "--domain", "example.test", "--qps", "1000000", "--tests", "4", "--timeout", "200ms"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReadResolvers(t *testing.T) {
	input := "\ufeff# comment\r\n 127.0.0.1 # local\n127.0.0.1:53\n::ffff:127.0.0.1\n[::1]\n::1\n[::1]:5353\nLOCALHOST.:053\nlocalhost\n\n"
	servers, err := readResolvers(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1:53", "[::1]:53", "[::1]:5353", "localhost:53"}
	if len(servers) != len(want) {
		t.Fatalf("got %v", servers)
	}
	for i := range want {
		if servers[i].address != want[i] {
			t.Errorf("got %s want %s", servers[i].address, want[i])
		}
	}
	for _, bad := range []string{"", "# empty", "localhost:0", "localhost:65536", "bad host", "[bad]", "a..b", "127.0.0.1:notaport"} {
		if _, err := readResolvers(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := readResolvers(failingReader{}); err == nil {
		t.Error("ignored scanner error")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestConfigValidation(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--in", "-", "--workers", "0"}, {"--in", "-", "--tests", "0"},
		{"--in", "-", "--qps", "NaN"}, {"--in", "-", "--timeout", "0s"},
		{"--in", "-", "--filter-time", "Inf"}, {"--in", "-", "--filter-rate", "101"},
		{"--in", "-", "--domain", "*.example.com"}, {"--in", "-", "--format", "xml"},
		{"--in", "-", "--include-filtered"}, {"--in", "-", "--format", "csv", "--save-dns=false"},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	c, err := parseConfig([]string{"--validation", "off", "--in", "-", "--domain", "EXAMPLE.COM.", "--save-dns=false"}, io.Discard)
	if err != nil || c.domain != "example.com." || c.format != "legacy-csv" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestMeasureCollectsAllResolvers(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		calls.Add(1)
		if m.Question[0].Name == "example.test." {
			replyWith(w, m, dns.RcodeSuccess)
			return
		}
		replyWith(w, m, dns.RcodeNameError)
	}, false)
	c := testConfig(t)
	c.workers = 8
	servers := make([]endpoint, 24)
	for i := range servers {
		servers[i] = server
	}
	results, err := measure(context.Background(), c, servers)
	if err != nil || len(results) != len(servers) {
		t.Fatalf("got %d results, %v", len(results), err)
	}
	for _, r := range results {
		if r.Filtered || r.Successes != c.tests || r.Failures != 0 || r.AverageMS < 0 || r.P95MS < r.P50MS {
			t.Errorf("bad result: %+v", r)
		}
	}
	if got, want := calls.Load(), int64(len(servers)*(c.tests+c.prechecks)); got != want {
		t.Fatalf("queries: got %d want %d", got, want)
	}
}

func TestPrecheckStopsAfterThreshold(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { calls.Add(1); replyWith(w, m, dns.RcodeServerFailure) }, false)
	c := testConfig(t)
	c.prechecks = 20
	c.precheckErrors = 1
	results, err := measure(context.Background(), c, []endpoint{server})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if calls.Load() != 2 || !r.Filtered || r.PrecheckFailures != 2 || r.Successes+r.Failures != 0 {
		t.Fatalf("calls=%d %+v", calls.Load(), r)
	}
	c.prechecks, c.precheckErrors = 1, 10
	results, err = measure(context.Background(), c, []endpoint{server})
	if err != nil || !results[0].Filtered {
		t.Fatal("a precheck with no successes must fail")
	}
}

func TestPrecheckToleratesOneFailure(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		if m.Question[0].Name == "example.test." {
			if calls.Add(1) == 1 {
				replyWith(w, m, dns.RcodeServerFailure)
			} else {
				replyWith(w, m, dns.RcodeSuccess)
			}
			return
		}
		replyWith(w, m, dns.RcodeNameError)
	}, false)
	results, err := measure(context.Background(), testConfig(t), []endpoint{server})
	if err != nil || results[0].Filtered || results[0].PrecheckFailures != 1 {
		t.Fatalf("%+v %v", results, err)
	}
}

func TestFailuresAndFilters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		truncated bool
		reason    string
	}{
		{"servfail", dns.RcodeServerFailure, false, "rcode_SERVFAIL"},
		{"wildcard", dns.RcodeSuccess, false, "rcode_NOERROR"},
		{"truncated", dns.RcodeNameError, true, "truncated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
				r := new(dns.Msg)
				r.SetReply(m)
				r.Rcode = tc.code
				r.Truncated = tc.truncated
				_ = w.WriteMsg(r)
			}, false)
			c := testConfig(t)
			c.prechecks = 0
			results, err := measure(context.Background(), c, []endpoint{server})
			if err != nil {
				t.Fatal(err)
			}
			r := results[0]
			if !r.Filtered || r.Failures != c.tests || r.Errors[tc.reason] != c.tests || r.Reasons[0] != "no_successful_queries" {
				t.Fatalf("%+v", r)
			}
		})
	}
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		time.Sleep(2 * time.Millisecond)
		if calls.Add(1)%3 == 0 {
			replyWith(w, m, dns.RcodeServerFailure)
		} else {
			replyWith(w, m, dns.RcodeNameError)
		}
	}, false)
	c := testConfig(t)
	c.prechecks = 0
	c.tests = 6
	c.minRate = 67
	c.maxErrors = 1
	c.maxTime = 0.000001
	results, err := measure(context.Background(), c, []endpoint{server})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Successes != 4 || r.Failures != 2 || len(r.Reasons) != 3 || r.SuccessRate <= 66 || r.SuccessRate >= 67 {
		t.Fatalf("%+v", r)
	}
}

func TestTCPFallback(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		calls.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		r.Rcode = dns.RcodeNameError
		r.Truncated = w.RemoteAddr().Network() == "udp"
		_ = w.WriteMsg(r)
	}, true)
	c := testConfig(t)
	c.prechecks = 0
	c.tcpFallback = true
	results, err := measure(context.Background(), c, []endpoint{server})
	if err != nil || results[0].Successes != c.tests || calls.Load() != int64(2*c.tests) {
		t.Fatalf("%+v %v calls=%d", results, err, calls.Load())
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	received := make(chan struct{}, 10)
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { received <- struct{}{} }, false)
	c := testConfig(t)
	c.prechecks = 0
	c.tests = 1
	c.timeout = 25 * time.Millisecond
	results, err := measure(context.Background(), c, []endpoint{server})
	if err != nil || results[0].Errors["timeout"] != 1 {
		t.Fatalf("%+v %v", results, err)
	}
	<-received
	c.timeout = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := measure(ctx, c, []endpoint{server}); done <- err }()
	<-received
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt DNS read")
	}
}

func TestGlobalRateLimit(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { calls.Add(1); replyWith(w, m, dns.RcodeNameError) }, false)
	c := testConfig(t)
	c.prechecks = 0
	c.tests = 2
	c.qps = 20
	start := time.Now()
	_, err := measure(context.Background(), c, []endpoint{server, server})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || time.Since(start) < 140*time.Millisecond {
		t.Fatalf("rate limit not shared: %d queries in %v", calls.Load(), time.Since(start))
	}
}

func TestOutputAndFilePreservation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "results.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := prepareOutput(target, "-")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = output.file.WriteString("partial")
	output.abort()
	data, _ := os.ReadFile(target)
	if string(data) != "original\n" {
		t.Fatal("abort changed target")
	}
	if _, err := prepareOutput(target, target); err == nil {
		t.Fatal("allowed input overwrite")
	}
	output, err = prepareOutput(target, "-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.abort()
	if err := writeResults(output.file, []resultStats{{Resolver: "::1"}}, "dns", false); err != nil {
		t.Fatal(err)
	}
	if err := output.commit(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "::1\n" {
		t.Fatalf("%q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary files leaked")
	}
}

func TestStructuredOutputAndSorting(t *testing.T) {
	results := []resultStats{
		{Resolver: "dead", Filtered: true, index: 0, Reasons: []string{"no_successful_queries"}, Errors: map[string]int{"timeout": 2}},
		{Resolver: "slow", Successes: 2, AverageMS: 2.125, SuccessRate: 100, index: 1},
		{Resolver: "fast", Successes: 1, AverageMS: 0.125, SuccessRate: 50, index: 2},
	}
	sortResults(results, "latency")
	if results[0].Resolver != "fast" || results[2].Resolver != "dead" {
		t.Fatal(results)
	}
	sortResults(results, "rate")
	if results[0].Resolver != "slow" {
		t.Fatal(results)
	}
	sortResults(results, "input")
	if results[0].Resolver != "dead" {
		t.Fatal(results)
	}
	var output bytes.Buffer
	if err := writeResults(&output, results, "json", true); err != nil {
		t.Fatal(err)
	}
	var decoded []resultStats
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || len(decoded) != 3 || decoded[2].AverageMS != 0.125 {
		t.Fatalf("%s %v", output.String(), err)
	}
	output.Reset()
	if err := writeResults(&output, results, "csv", false); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&output).ReadAll()
	if err != nil || len(rows) != 3 || rows[0][0] != "resolver" || rows[2][1] != "0.125" {
		t.Fatalf("%v %v", rows, err)
	}
	output.Reset()
	_ = writeResults(&output, nil, "json", false)
	if strings.TrimSpace(output.String()) != "[]" {
		t.Fatal(output.String())
	}
	if percentile([]float64{0.1, 0.2, 0.3, 0.4}, 0.5) != 0.2 || percentile([]float64{0.1, 0.2, 0.3, 0.4}, 0.95) != 0.4 {
		t.Fatal("incorrect percentile")
	}
	for _, format := range []string{"dns", "csv", "json", "legacy-csv"} {
		if err := writeResults(failingWriter{}, results, format, false); err == nil {
			t.Errorf("ignored %s write error", format)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRunExitCodesAndStdout(t *testing.T) {
	var out, report bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(""), &out, &report); code != 2 {
		t.Fatal(code)
	}
	if code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &report); code != 0 {
		t.Fatal(code)
	}
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { replyWith(w, m, dns.RcodeNameError) }, false)
	args := []string{"--validation", "off", "--in", "-", "--out", "-", "--format", "json", "--tests", "2", "--precheck-tests", "0", "--qps", "1000000"}
	out.Reset()
	report.Reset()
	if code := run(context.Background(), args, strings.NewReader(server.label), &out, &report); code != 0 {
		t.Fatalf("%d %s", code, report.String())
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(report.String(), "STATUS") {
		t.Fatalf("stdout=%s stderr=%s", out.String(), report.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx, args, strings.NewReader(server.label), &out, &report); code != 130 {
		t.Fatal(code)
	}
}

func TestRunPreservesOutputOnCancellationAndErrors(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "results.txt")
	if err := os.WriteFile(target, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 1)
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { received <- struct{}{} }, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--validation", "off", "--in", "-", "--out", target, "--precheck-tests", "0", "--tests", "1", "--timeout", "10s"}, strings.NewReader(server.label), io.Discard, io.Discard)
	}()
	<-received
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not cancel")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "previous\n" {
		t.Fatal("cancellation overwrote output")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("cancellation leaked temporary output")
	}
	// A bad destination must fail before any queries are scheduled.
	code := run(context.Background(), []string{"--validation", "off", "--in", "-", "--out", filepath.Join(dir, "missing", "results.txt")}, strings.NewReader(server.label), io.Discard, io.Discard)
	if code != 1 {
		t.Fatal(code)
	}
	select {
	case <-received:
		t.Fatal("queried despite invalid destination")
	default:
	}
}

func TestRunWritesEmptyExportWhenAllResolversFail(t *testing.T) {
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { replyWith(w, m, dns.RcodeServerFailure) }, false)
	target := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(target, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	code := run(context.Background(), []string{"--validation", "off", "--in", "-", "--out", target, "--format", "json", "--quiet", "--tests", "1", "--precheck-tests", "0"}, strings.NewReader(server.label), io.Discard, io.Discard)
	if code != 1 {
		t.Fatal(code)
	}
	data, _ := os.ReadFile(target)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("%s", data)
	}
}

func TestTCPRetryRespectsQueryBudget(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		calls.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		r.Truncated = true
		_ = w.WriteMsg(r)
	}, true)
	c := testConfig(t)
	c.prechecks = 0
	c.tests = 1
	c.qps = 1
	c.timeout = 20 * time.Millisecond
	c.tcpFallback = true
	results, err := measure(context.Background(), c, []endpoint{server})
	if err != nil || calls.Load() != 1 || results[0].Errors["timeout"] != 1 {
		t.Fatalf("%+v %v calls=%d", results, err, calls.Load())
	}
}

func TestIPv6Resolver(t *testing.T) {
	packet, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	ready := make(chan struct{})
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) { replyWith(w, m, dns.RcodeNameError) }), NotifyStartedFunc: func() { close(ready) }}
	go func() {
		if err := server.ActivateAndServe(); err != nil {
			t.Errorf("serve IPv6: %v", err)
		}
	}()
	<-ready
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	ep, err := parseEndpoint(packet.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(t)
	c.prechecks = 0
	results, err := measure(context.Background(), c, []endpoint{ep})
	if err != nil || results[0].Successes != c.tests {
		t.Fatalf("%+v %v", results, err)
	}
}

func BenchmarkReadResolvers(b *testing.B) {
	var input strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&input, "127.0.0.1:%d\n", 10000+i)
	}
	data := input.String()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := readResolvers(strings.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLocalResolver(b *testing.B) {
	server := localDNS(b, func(w dns.ResponseWriter, m *dns.Msg) { replyWith(w, m, dns.RcodeNameError) }, false)
	c := testConfig(b)
	c.prechecks = 0
	names := []string{"a.example.test.", "b.example.test.", "c.example.test.", "d.example.test."}
	client := queryClient{udp: dns.Client{Net: "udp", Timeout: time.Second}, limiter: rate.NewLimiter(rate.Inf, 1), timeout: time.Second}
	samples := make([]float64, 0, c.tests)
	b.ReportAllocs()
	for b.Loop() {
		r := checkResolver(context.Background(), c, server, names, &client, samples, validationPlan{})
		if r.Successes != c.tests {
			b.Fatal(r)
		}
	}
}

// Isolate the cost of the previous and current precheck settings on the same
// implementation and local server. This is not a historical binary comparison.
func BenchmarkPrecheckBudget(b *testing.B) {
	server := localDNS(b, func(w dns.ResponseWriter, m *dns.Msg) {
		code := dns.RcodeNameError
		if m.Question[0].Name == "example.test." {
			code = dns.RcodeSuccess
		}
		replyWith(w, m, code)
	}, false)
	for _, prechecks := range []int{20, 3} {
		b.Run(fmt.Sprintf("prechecks_%d", prechecks), func(b *testing.B) {
			c := testConfig(b)
			c.prechecks = prechecks
			c.tests = 10
			names := make([]string, c.tests)
			for i := range names {
				names[i] = fmt.Sprintf("%d.example.test.", i)
			}
			client := queryClient{udp: dns.Client{Net: "udp", Timeout: time.Second}, limiter: rate.NewLimiter(rate.Inf, 1), timeout: time.Second}
			samples := make([]float64, 0, c.tests)
			b.ReportAllocs()
			for b.Loop() {
				r := checkResolver(context.Background(), c, server, names, &client, samples, validationPlan{})
				if r.Successes != c.tests {
					b.Fatal(r)
				}
			}
			b.ReportMetric(float64(prechecks+c.tests), "queries/op")
		})
	}
}
