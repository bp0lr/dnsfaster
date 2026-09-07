package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/time/rate"
)

func validationConfig(t testing.TB, args ...string) config {
	t.Helper()
	flags := []string{"--resolver", "127.0.0.1:5353", "--domain", "example.test", "--negative-domain", "negative.test", "--qps", "1000000", "--tests", "2", "--precheck-tests", "0", "--timeout", "200ms"}
	c, err := parseConfig(append(flags, args...), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func replyAddresses(w dns.ResponseWriter, m *dns.Msg, addresses ...string) {
	r := new(dns.Msg)
	r.SetReply(m)
	for _, address := range addresses {
		r.Answer = append(r.Answer, &dns.A{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(address)})
	}
	_ = w.WriteMsg(r)
}

func referenceServer(t testing.TB, addresses []string, calls *atomic.Int64) endpoint {
	return localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		if calls != nil {
			calls.Add(1)
		}
		switch m.Question[0].Name {
		case "example.test.", "second.test.":
			replyAddresses(w, m, addresses...)
		default:
			replyWith(w, m, dns.RcodeNameError)
		}
	}, false)
}

func TestReferenceQuorumAndCandidateValidation(t *testing.T) {
	var referenceCalls, candidateCalls atomic.Int64
	a := referenceServer(t, []string{"192.0.2.2", "192.0.2.1"}, &referenceCalls)
	b := referenceServer(t, []string{"192.0.2.1", "192.0.2.2"}, &referenceCalls)
	c := referenceServer(t, []string{"192.0.2.99"}, &referenceCalls)
	good := referenceServer(t, []string{"192.0.2.1", "192.0.2.2"}, &candidateCalls)
	bad := referenceServer(t, []string{"192.0.2.99"}, &candidateCalls)
	cfg := validationConfig(t, "--baseline", a.label+","+b.label+","+c.label, "--positive-domain", "second.test")
	servers := []endpoint{good, bad, good, good}
	results, err := measure(context.Background(), cfg, servers)
	if err != nil {
		t.Fatal(err)
	}
	sortResults(results, "input")
	for i, r := range results {
		if i == 1 {
			if !r.Filtered || r.ValidationFailures != 1 || r.Errors["validation_positive_mismatch"] != 1 || r.Successes+r.Failures != 0 {
				t.Fatalf("bad candidate accepted: %+v", r)
			}
		} else if r.Filtered || r.ValidationChecks != 4 || r.ValidationFailures != 0 || r.Successes != cfg.tests {
			t.Fatalf("good candidate rejected: %+v", r)
		}
	}
	// Two positive domains and two negative names are checked once per reference,
	// regardless of candidate count. Negative measurement names are separate.
	if referenceCalls.Load() < 8 || referenceCalls.Load() > 12 {
		t.Fatalf("reference queries=%d", referenceCalls.Load())
	}
	if candidateCalls.Load() != 19 {
		t.Fatalf("candidate queries=%d", candidateCalls.Load())
	}
}

func TestReferenceFailureDoesNotQueryCandidates(t *testing.T) {
	var candidateCalls atomic.Int64
	a := referenceServer(t, []string{"192.0.2.1"}, nil)
	b := referenceServer(t, []string{"192.0.2.2"}, nil)
	candidate := referenceServer(t, []string{"192.0.2.1"}, &candidateCalls)
	cfg := validationConfig(t, "--baseline", a.label+","+b.label)
	_, err := measure(context.Background(), cfg, []endpoint{candidate})
	if err == nil || !strings.Contains(err.Error(), "quorum") || candidateCalls.Load() != 0 {
		t.Fatalf("err=%v candidate queries=%d", err, candidateCalls.Load())
	}
}

func TestReferenceNegativeChecksMustReachQuorum(t *testing.T) {
	var candidateCalls atomic.Int64
	wildcard := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { replyAddresses(w, m, "192.0.2.1") }, false)
	candidate := referenceServer(t, []string{"192.0.2.1"}, &candidateCalls)
	cfg := validationConfig(t, "--baseline", wildcard.label)
	_, err := measure(context.Background(), cfg, []endpoint{candidate})
	if err == nil || candidateCalls.Load() != 0 {
		t.Fatalf("err=%v queries=%d", err, candidateCalls.Load())
	}
}

func TestMissingReferenceAnswersFailClosed(t *testing.T) {
	empty := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { replyWith(w, m, dns.RcodeSuccess) }, false)
	cfg := validationConfig(t, "--baseline", empty.label)
	_, err := prepareValidation(context.Background(), cfg, rate.NewLimiter(rate.Inf, 1))
	if err == nil {
		t.Fatal("empty reference A answers accepted")
	}
}

func TestExpectedModeAndNegativeMismatch(t *testing.T) {
	reference := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		t.Error("expected mode contacted a reference")
		replyWith(w, m, dns.RcodeServerFailure)
	}, false)
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
				if m.Question[0].Name == "example.test." || (bad && strings.HasSuffix(m.Question[0].Name, ".negative.test.")) {
					replyAddresses(w, m, "192.0.2.1")
				} else {
					replyWith(w, m, dns.RcodeNameError)
				}
			}, false)
			cfg := validationConfig(t, "--validation", "expected", "--expect", "example.test=192.0.2.1", "--baseline", reference.label)
			results, err := measure(context.Background(), cfg, []endpoint{server})
			if err != nil {
				t.Fatal(err)
			}
			r := results[0]
			if r.Filtered != bad || r.ValidationChecks != 3 {
				t.Fatalf("%+v", r)
			}
			if bad && (r.Errors["validation_negative_rcode_NOERROR"] != 1 || r.Successes+r.Failures != 0) {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestAnswerAddresses(t *testing.T) {
	parse := func(records ...string) *dns.Msg {
		r := new(dns.Msg)
		for _, text := range records {
			rr, err := dns.NewRR(text)
			if err != nil {
				t.Fatal(err)
			}
			r.Answer = append(r.Answer, rr)
		}
		return r
	}
	answer, reason := answerAddresses(parse("EXAMPLE.TEST. 10 IN CNAME Alias.test.", "alias.test. 20 IN A 192.0.2.2", "alias.test. 90 IN A 192.0.2.1", "alias.test. 90 IN A 192.0.2.1", "unrelated.test. 1 IN A 192.0.2.99"), "example.test.", dns.TypeA)
	if reason != "" || strings.Join(answer, ",") != "192.0.2.1,192.0.2.2" {
		t.Fatalf("%v %s", answer, reason)
	}
	for _, tc := range []struct {
		records []string
		want    string
	}{
		{nil, "no_address_answers"},
		{[]string{"unrelated.test. 1 IN A 192.0.2.1"}, "no_address_answers"},
		{[]string{"example.test. 1 IN CNAME alias.test.", "alias.test. 1 IN CNAME example.test."}, "cname_loop"},
		{[]string{"example.test. 1 IN CNAME alias.test.", "example.test. 1 IN A 192.0.2.1"}, "invalid_cname"},
	} {
		_, reason := answerAddresses(parse(tc.records...), "example.test.", dns.TypeA)
		if reason != tc.want {
			t.Fatalf("%s want %s", reason, tc.want)
		}
	}
}

func TestValidationArguments(t *testing.T) {
	base := []string{"--resolver", "127.0.0.1", "--domain", "example.test"}
	for _, args := range [][]string{
		{"--validation", "unknown"}, {"--baseline", "127.0.0.1,127.0.0.1:53", "--baseline-quorum", "2"},
		{"--baseline", "127.0.0.1,127.0.0.2,127.0.0.3", "--baseline-quorum", "1"},
		{"--validation", "expected"}, {"--validation", "off", "--expect", "example.test=192.0.2.1"},
		{"--validation", "expected", "--expect", "example.test=::1"},
		{"--expect", "other.test=192.0.2.1"}, {"--expect", "example.test="},
		{"--query-prefix", "has.dots"}, {"--query-prefix", strings.Repeat("a", 47)},
		{"--negative-domain", "*.test"}, {"--positive-domain", "bad name"},
		{"--top", "-1"}, {"--max-duration", "-1s"}, {"--filter-p95", "NaN"},
	} {
		if _, err := parseConfig(append(slicesClone(base), args...), io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	cfg := validationConfig(t, "--validation", "expected", "--expect", "example.test=192.0.2.1,192.0.2.1", "--query-prefix", "Health")
	if cfg.queryPrefix != "health" {
		t.Fatal(cfg.queryPrefix)
	}
	name := randomName(cfg.domain, cfg.queryPrefix)
	if !strings.HasPrefix(name, "health-") || !strings.HasSuffix(name, ".example.test.") {
		t.Fatal(name)
	}
}

func slicesClone(values []string) []string { return append([]string(nil), values...) }

func TestValidationCancellation(t *testing.T) {
	received := make(chan struct{}, 8)
	silent := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { received <- struct{}{} }, false)
	cfg := validationConfig(t, "--baseline", silent.label, "--timeout", "10s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := prepareValidation(ctx, cfg, rate.NewLimiter(rate.Inf, 1)); done <- err }()
	<-received
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reference queries did not cancel")
	}
}

func TestSourcesAndExclusions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/resolvers":
			fmt.Fprintln(w, "127.0.0.1\n127.0.0.1:53\n127.0.0.1:5353\n127.0.0.2\n[::1]:5353\nlocalhost:5353")
		case "/exclude":
			fmt.Fprintln(w, "# exclusions\n127.0.0.1:5353\n127.0.0.2/32\n::1")
		case "/redirect":
			http.Redirect(w, r, "/resolvers", http.StatusFound)
		case "/large":
			w.Header().Set("Content-Length", fmt.Sprint(maxSourceBytes+1))
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := parseConfig([]string{"--in", server.URL + "/redirect", "--resolver", "127.0.0.3", "--exclude-file", server.URL + "/exclude", "--exclude", "LOCALHOST.", "--validation", "off"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	resolvers, excluded, err := loadResolvers(context.Background(), c, strings.NewReader(""))
	if err != nil || excluded != 4 || len(resolvers) != 2 || resolvers[0].label != "127.0.0.1" || resolvers[1].label != "127.0.0.3" {
		t.Fatalf("%v excluded=%d err=%v", resolvers, excluded, err)
	}
	for _, path := range []string{"/missing", "/large"} {
		if source, err := openSource(context.Background(), server.URL+path); err == nil {
			source.Close()
			t.Errorf("accepted %s", path)
		}
	}
	if _, err := parseExclusions([]string{"127.0.0.1/99"}); err == nil {
		t.Fatal("accepted invalid CIDR")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if source, err := openSource(ctx, server.URL+"/resolvers"); err == nil {
		source.Close()
		t.Fatal("canceled download succeeded")
	}
	c.exclusions = []string{"127.0.0.0/8", "::1", "localhost"}
	if _, _, err := loadResolvers(context.Background(), c, strings.NewReader("")); err == nil {
		t.Fatal("accepted empty selection")
	}
}

func TestBareHostExclusionCoversEveryPort(t *testing.T) {
	set, err := parseExclusions([]string{"127.0.0.1", "[::1]", "localhost", "::ffff:192.0.2.0/120"})
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"127.0.0.1:5353", "[::1]:5353", "localhost:5353", "192.0.2.1"} {
		ep, err := parseEndpoint(address)
		if err != nil {
			t.Fatal(err)
		}
		if !set.contains(ep) {
			t.Errorf("did not exclude %s", address)
		}
	}
}

func TestTopP95AndProgress(t *testing.T) {
	results := []resultStats{
		{Resolver: "average-winner", AverageMS: 1, P95MS: 20, Successes: 10, SuccessRate: 100, index: 0},
		{Resolver: "p95-winner", AverageMS: 2, P95MS: 3, Successes: 10, SuccessRate: 100, index: 1},
		{Resolver: "rejected", Filtered: true, index: 2},
	}
	sortResults(results, "p95")
	selected := selectExports(results, 1, true)
	if len(selected) != 2 || selected[0].Resolver != "p95-winner" || selected[1].Resolver != "rejected" {
		t.Fatal(selected)
	}
	selected = selectExports(results, 1, false)
	if len(selected) != 1 || selected[0].Resolver != "p95-winner" {
		t.Fatal(selected)
	}
	var report bytes.Buffer
	p := &progressReporter{writer: &report, started: time.Now(), enabled: true, verbose: true, total: 3}
	if err := p.phase("start"); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if err := p.complete(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.tick(); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(&report, results, 4, 1, p.started); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3/3 checked", "2 passed", "4 excluded", "1 selected", "FILTER rejected"} {
		if !strings.Contains(report.String(), want) {
			t.Fatalf("missing %q in %s", want, report.String())
		}
	}
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		time.Sleep(2 * time.Millisecond)
		replyWith(w, m, dns.RcodeNameError)
	}, false)
	c := testConfig(t)
	c.prechecks = 0
	c.maxP95 = 0.000001
	measured, err := measure(context.Background(), c, []endpoint{server})
	if err != nil || !measured[0].Filtered || !strings.Contains(strings.Join(measured[0].Reasons, ","), "p95_latency") {
		t.Fatalf("%v %v", measured, err)
	}
}

func TestReferenceFailurePreservesOutput(t *testing.T) {
	a := referenceServer(t, []string{"192.0.2.1"}, nil)
	b := referenceServer(t, []string{"192.0.2.2"}, nil)
	target := filepath.Join(t.TempDir(), "results.txt")
	if err := os.WriteFile(target, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	code := run(context.Background(), []string{"--resolver", "127.0.0.1:5353", "--domain", "example.test", "--negative-domain", "example.test", "--baseline", a.label + "," + b.label, "--out", target}, strings.NewReader(""), io.Discard, &report)
	if code != 1 || !strings.Contains(report.String(), "quorum") {
		t.Fatalf("code=%d %s", code, report.String())
	}
	data, _ := os.ReadFile(target)
	if string(data) != "previous\n" {
		t.Fatalf("overwrote output: %s", data)
	}
}

func TestWholeRunDeadline(t *testing.T) {
	silent := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {}, false)
	var report bytes.Buffer
	start := time.Now()
	code := run(context.Background(), []string{"--resolver", silent.label, "--validation", "off", "--timeout", "10s", "--max-duration", "30ms"}, strings.NewReader(""), io.Discard, &report)
	if code != 1 || time.Since(start) > time.Second || !strings.Contains(report.String(), "deadline exceeded") {
		t.Fatalf("code=%d elapsed=%s %s", code, time.Since(start), report.String())
	}
}

func TestWholeRunRateBudgetPreservesOutput(t *testing.T) {
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { replyWith(w, m, dns.RcodeNameError) }, false)
	target := filepath.Join(t.TempDir(), "results.txt")
	if err := os.WriteFile(target, []byte("previous\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	code := run(context.Background(), []string{"--resolver", server.label, "--validation", "off", "--precheck-tests", "0", "--tests", "10", "--qps", "1", "--max-duration", "30ms", "--out", target}, strings.NewReader(""), io.Discard, &report)
	if code != 1 || !strings.Contains(report.String(), "deadline exceeded") {
		t.Fatalf("code=%d %s", code, report.String())
	}
	data, _ := os.ReadFile(target)
	if string(data) != "previous\n" {
		t.Fatalf("overwrote output: %s", data)
	}
}

func TestInstalledVersionAndStdout(t *testing.T) {
	for _, tc := range []struct {
		info *debug.BuildInfo
		want string
	}{
		{nil, "dev"},
		{&debug.BuildInfo{Main: debug.Module{Version: "v1.0.0"}}, "v1.0.0"},
		{&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}}, "abc123+dirty"},
	} {
		if got := buildVersion(tc.info); got != tc.want {
			t.Fatalf("%s want %s", got, tc.want)
		}
	}
	var out, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, strings.NewReader(""), &out, &stderr); code != 0 || out.Len() == 0 || stderr.Len() != 0 {
		t.Fatalf("%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestExpectedCLIExportAndQuiet(t *testing.T) {
	a := referenceServer(t, []string{"192.0.2.1"}, nil)
	b := referenceServer(t, []string{"192.0.2.1"}, nil)
	var out, report bytes.Buffer
	args := []string{"--resolver", a.label + "," + b.label, "--domain", "example.test", "--negative-domain", "example.test", "--validation", "expected", "--expect", "example.test=192.0.2.1", "--precheck-tests", "0", "--tests", "1", "--top", "1", "--sort", "p95", "--out", "-", "--quiet", "--qps", "1000000"}
	if code := run(context.Background(), args, strings.NewReader(""), &out, &report); code != 0 {
		t.Fatalf("%d %s", code, report.String())
	}
	if strings.Count(out.String(), "\n") != 1 || report.Len() != 0 {
		t.Fatalf("stdout=%s stderr=%s", out.String(), report.String())
	}
}

func BenchmarkValidatedResolver(b *testing.B) {
	server := referenceServer(b, []string{"192.0.2.1"}, nil)
	cfg := validationConfig(b, "--validation", "expected", "--expect", "example.test=192.0.2.1")
	limiter := rate.NewLimiter(rate.Inf, 1)
	plan, err := prepareValidation(context.Background(), cfg, limiter)
	if err != nil {
		b.Fatal(err)
	}
	client := newQueryClient(cfg, limiter)
	names := []string{"one.example.test.", "two.example.test."}
	samples := make([]float64, 0, cfg.tests)
	b.ReportAllocs()
	for b.Loop() {
		r := checkResolver(context.Background(), cfg, server, names, client, samples, plan)
		if r.Filtered {
			b.Fatal(r)
		}
	}
	b.ReportMetric(5, "queries/op")
}

func TestAdaptivePrechecks(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, 0},
		{[]string{"--validation", "off"}, 3},
		{[]string{"--validation", "expected", "--expect", "example.com=192.0.2.1"}, 0},
		{[]string{"--precheck-tests", "4"}, 4},
		{[]string{"--validation", "off", "--precheck-tests", "0"}, 0},
	} {
		c, err := parseConfig(append([]string{"--resolver", "127.0.0.1"}, tc.args...), io.Discard)
		if err != nil || c.prechecks != tc.want {
			t.Fatalf("%v: prechecks=%d err=%v", tc.args, c.prechecks, err)
		}
	}
}

func TestReferenceQuorumCancelsSilentMinority(t *testing.T) {
	silentStarted := make(chan struct{}, 1)
	silent := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		select {
		case silentStarted <- struct{}{}:
		default:
		}
	}, false)
	goodHandler := func(w dns.ResponseWriter, m *dns.Msg) {
		select {
		case <-silentStarted:
			silentStarted <- struct{}{}
		case <-time.After(time.Second):
		}
		if m.Question[0].Name == "example.test." {
			replyAddresses(w, m, "192.0.2.1")
		} else {
			replyWith(w, m, dns.RcodeNameError)
		}
	}
	a := localDNS(t, goodHandler, false)
	b := localDNS(t, goodHandler, false)
	candidate := referenceServer(t, []string{"192.0.2.1"}, nil)
	cfg := validationConfig(t, "--baseline", silent.label+","+a.label+","+b.label, "--timeout", "10s")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results, err := measure(ctx, cfg, []endpoint{candidate})
	if err != nil || len(results) != 1 || results[0].Filtered || results[0].Successes != 2 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

func TestValidationRetriesAndMeasurementIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retries   int
		drop      int64
		wantPass  bool
		wantCalls int64
	}{
		{"recover", 1, 1, true, 2}, {"disabled", 0, 1, false, 1}, {"bounded", 2, 100, false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var positiveCalls atomic.Int64
			server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
				if m.Question[0].Name == "example.test." {
					if positiveCalls.Add(1) <= tc.drop {
						return
					}
					replyAddresses(w, m, "192.0.2.1")
				} else {
					replyWith(w, m, dns.RcodeNameError)
				}
			}, false)
			cfg := validationConfig(t, "--validation", "expected", "--expect", "example.test=192.0.2.1", "--validation-retries", fmt.Sprint(tc.retries), "--timeout", "30ms")
			results, err := measure(context.Background(), cfg, []endpoint{server})
			if err != nil {
				t.Fatal(err)
			}
			r := results[0]
			if r.Filtered == tc.wantPass || positiveCalls.Load() != tc.wantCalls || r.ValidationRetries != int(tc.wantCalls)-1 {
				t.Fatalf("%+v calls=%d", r, positiveCalls.Load())
			}
		})
	}
	var calls atomic.Int64
	silent := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { calls.Add(1) }, false)
	cfg := validationConfig(t, "--validation", "off", "--validation-retries", "3", "--timeout", "30ms")
	results, err := measure(context.Background(), cfg, []endpoint{silent})
	if err != nil || calls.Load() != 2 || results[0].Failures != 2 || results[0].ValidationRetries != 0 {
		t.Fatalf("%+v calls=%d err=%v", results, calls.Load(), err)
	}
}

func TestValidationDoesNotRetryBadAnswers(t *testing.T) {
	for _, code := range []int{dns.RcodeSuccess, dns.RcodeServerFailure, dns.RcodeRefused} {
		var calls atomic.Int64
		server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
			calls.Add(1)
			if code == dns.RcodeSuccess {
				replyAddresses(w, m, "192.0.2.99")
			} else {
				replyWith(w, m, code)
			}
		}, false)
		cfg := validationConfig(t, "--validation", "expected", "--expect", "example.test=192.0.2.1", "--validation-retries", "3")
		results, err := measure(context.Background(), cfg, []endpoint{server})
		if err != nil || !results[0].Filtered || calls.Load() != 1 || results[0].ValidationRetries != 0 {
			t.Fatalf("%+v calls=%d err=%v", results, calls.Load(), err)
		}
	}
}

func TestReferenceRetries(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
		if calls.Add(1) == 1 {
			return
		}
		if m.Question[0].Name == "example.test." {
			replyAddresses(w, m, "192.0.2.1")
		} else {
			replyWith(w, m, dns.RcodeNameError)
		}
	}, false)
	cfg := validationConfig(t, "--baseline", server.label, "--timeout", "30ms")
	_, err := prepareValidation(context.Background(), cfg, rate.NewLimiter(rate.Inf, 1))
	if err != nil || calls.Load() != 4 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestDualStackValidation(t *testing.T) {
	serverFor := func(badAAAA, badNegative bool) endpoint {
		return localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {
			q := m.Question[0]
			if q.Name != "example.test." {
				if badNegative && q.Qtype == dns.TypeAAAA {
					replyWith(w, m, dns.RcodeSuccess)
				} else {
					replyWith(w, m, dns.RcodeNameError)
				}
				return
			}
			if q.Qtype == dns.TypeA {
				replyAddresses(w, m, "192.0.2.1")
				return
			}
			r := new(dns.Msg)
			r.SetReply(m)
			address := "2001:db8::1"
			if badAAAA {
				address = "2001:db8::99"
			}
			for _, text := range []string{"example.test. 60 IN CNAME alias.test.", "alias.test. 10 IN AAAA " + address, "unrelated.test. 50 IN AAAA 2001:db8::ffff"} {
				rr, err := dns.NewRR(text)
				if err != nil {
					t.Error(err)
					return
				}
				r.Answer = append(r.Answer, rr)
			}
			_ = w.WriteMsg(r)
		}, false)
	}
	good, bad, badNegative := serverFor(false, false), serverFor(true, false), serverFor(false, true)
	for _, mode := range []string{"baseline", "expected"} {
		args := []string{"--record-types", "AAAA,a,AAAA", "--validation", mode, "--baseline", good.label}
		if mode == "expected" {
			args = append(args, "--expect", "example.test=192.0.2.1,2001:0db8:0::1")
		}
		cfg := validationConfig(t, args...)
		results, err := measure(context.Background(), cfg, []endpoint{good, bad, badNegative})
		if err != nil {
			t.Fatal(err)
		}
		sortResults(results, "input")
		if results[0].Filtered || results[0].ValidationChecks != 6 || !results[1].Filtered || !results[2].Filtered {
			t.Fatalf("%s: %+v", mode, results)
		}
		if !strings.Contains(results[1].Reasons[0], "/AAAA") || results[1].ValidationRetries != 0 {
			t.Fatal(results[1])
		}
	}
	cfg := validationConfig(t, "--record-types", "AAAA", "--validation", "expected", "--expect", "example.test=2001:db8::1")
	results, err := measure(context.Background(), cfg, []endpoint{good})
	if err != nil || results[0].Filtered || results[0].ValidationChecks != 3 {
		t.Fatalf("%+v %v", results, err)
	}
}

func TestNewValidationArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--record-types", ""}, {"--record-types", "TXT"}, {"--validation-retries", "4"}, {"--validation-retries", "-1"},
		{"--record-types", "A,AAAA", "--validation", "expected", "--expect", "example.com=192.0.2.1"},
		{"--record-types", "AAAA", "--expect", "example.com=fe80::1%eth0"},
	} {
		if _, err := parseConfig(append([]string{"--resolver", "127.0.0.1"}, args...), io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestReferenceImpossibleQuorumStopsEarly(t *testing.T) {
	a := referenceServer(t, []string{"192.0.2.1"}, nil)
	b := referenceServer(t, []string{"192.0.2.2"}, nil)
	silent := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) {}, false)
	cfg := validationConfig(t, "--baseline", a.label+","+b.label+","+silent.label, "--baseline-quorum", "3", "--timeout", "10s")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := prepareValidation(ctx, cfg, rate.NewLimiter(rate.Inf, 1))
	if err == nil || !strings.Contains(err.Error(), "quorum") || ctx.Err() != nil {
		t.Fatalf("err=%v ctx=%v", err, ctx.Err())
	}
}

func TestValidationRetryRateAndDeadline(t *testing.T) {
	var calls atomic.Int64
	silent := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { calls.Add(1) }, false)
	cfg := validationConfig(t, "--validation", "expected", "--expect", "example.test=192.0.2.1", "--validation-retries", "3", "--timeout", "20ms")
	client := newQueryClient(cfg, rate.NewLimiter(10, 1))
	var msg dns.Msg
	msg.SetQuestion("example.test.", dns.TypeA)
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Millisecond)
	defer cancel()
	_, reason, _ := client.validationLookup(ctx, &msg, silent.address, dns.RcodeSuccess)
	if reason != "canceled" || ctx.Err() == nil || calls.Load() > 2 {
		t.Fatalf("reason=%s calls=%d ctx=%v", reason, calls.Load(), ctx.Err())
	}
}

func TestAAAAAddressSets(t *testing.T) {
	for _, tc := range []struct {
		records      []string
		want, reason string
	}{
		{[]string{"example.test. 10 IN AAAA 2001:0db8:0::1", "example.test. 60 IN AAAA 2001:db8::1", "example.test. 10 IN A 192.0.2.1"}, "2001:db8::1", ""},
		{[]string{"example.test. 10 IN AAAA ::ffff:192.0.2.1"}, "::ffff:192.0.2.1", ""},
		{[]string{"example.test. 10 IN A 192.0.2.1"}, "", "no_address_answers"},
		{[]string{"example.test. 10 IN CNAME alias.test.", "example.test. 10 IN AAAA 2001:db8::1"}, "", "invalid_cname"},
	} {
		msg := new(dns.Msg)
		for _, text := range tc.records {
			rr, err := dns.NewRR(text)
			if err != nil {
				t.Fatal(err)
			}
			msg.Answer = append(msg.Answer, rr)
		}
		addresses, reason := answerAddresses(msg, "example.test.", dns.TypeAAAA)
		if reason != tc.reason || strings.Join(addresses, ",") != tc.want {
			t.Fatalf("%v %s want %s %s", addresses, reason, tc.want, tc.reason)
		}
	}
}

func TestMalformedValidationResponseIsNotRetried(t *testing.T) {
	var calls atomic.Int64
	server := localDNS(t, func(w dns.ResponseWriter, m *dns.Msg) { calls.Add(1); _, _ = w.Write([]byte{0}) }, false)
	cfg := validationConfig(t, "--validation", "expected", "--expect", "example.test=192.0.2.1", "--validation-retries", "3")
	results, err := measure(context.Background(), cfg, []endpoint{server})
	if err != nil || calls.Load() != 1 || results[0].Errors["validation_invalid_response"] != 1 || results[0].ValidationRetries != 0 {
		t.Fatalf("%+v calls=%d err=%v", results, calls.Load(), err)
	}
}
