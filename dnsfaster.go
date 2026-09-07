package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	flag "github.com/spf13/pflag"
)

var version = "dev"

type config struct {
	input, output, domain, format, sortBy                string
	workers, tests, prechecks, precheckErrors, maxErrors int
	timeout                                              time.Duration
	maxTime, minRate, qps                                float64
	tcpFallback, quiet, includeFiltered                  bool
	validation, excludeFile, queryPrefix                 string
	resolverInputs, exclusions, baselineInputs           []string
	positiveDomains, negativeDomains, expectedInputs     []string
	baselines                                            []endpoint
	quorum, top                                          int
	maxP95                                               float64
	maxDuration                                          time.Duration
	progress, verbose, showVersion                       bool
	recordTypes                                          []string
	validationRetries                                    int
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	var c config
	var saveDNS bool
	fs := flag.NewFlagSet("dnsfaster", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.input, "in", "", "Resolver file, HTTP(S) URL, or - for stdin")
	fs.StringSliceVar(&c.resolverInputs, "resolver", nil, "Resolver endpoints (repeatable or comma-separated)")
	fs.StringSliceVar(&c.exclusions, "exclude", nil, "Exclude hosts, endpoints or CIDRs (repeatable)")
	fs.StringVar(&c.excludeFile, "exclude-file", "", "Exclusion file or HTTP(S) URL")
	fs.StringVar(&c.output, "out", "", "Output file, or - for stdout")
	fs.StringVar(&c.domain, "domain", "example.com", "Base domain without wildcard DNS")
	fs.IntVar(&c.workers, "workers", 10, "Concurrent resolver checks (1-251)")
	fs.IntVar(&c.tests, "tests", 10, "Measurements per resolver (1-5000)")
	fs.DurationVar(&c.timeout, "timeout", 2*time.Second, "Timeout per query, including TCP fallback")
	fs.Float64Var(&c.qps, "qps", 50, "Global query rate limit, including prechecks and TCP retries")
	fs.IntVar(&c.prechecks, "precheck-tests", 3, "Extra base-domain checks (default 0 with validation, 3 when off; maximum 1000)")
	fs.IntVar(&c.precheckErrors, "precheck-errors", 1, "Allowed failures during prechecks")
	fs.Float64Var(&c.maxTime, "filter-time", 0, "Maximum mean latency in milliseconds (0 disables)")
	fs.Float64Var(&c.maxP95, "filter-p95", 0, "Maximum p95 latency in milliseconds (0 disables)")
	fs.IntVar(&c.maxErrors, "filter-errors", 0, "Maximum measurement failures (0 disables)")
	fs.Float64Var(&c.minRate, "filter-rate", 0, "Minimum success percentage (0 disables)")
	fs.BoolVar(&c.tcpFallback, "tcp-fallback", false, "Retry truncated UDP responses over TCP")
	fs.BoolVar(&c.quiet, "quiet", false, "Suppress the console report")
	fs.BoolVar(&c.progress, "progress", true, "Show periodic progress on stderr")
	fs.BoolVar(&c.verbose, "verbose", false, "Report each completed resolver on stderr")
	fs.IntVar(&c.top, "top", 0, "Export at most this many passing resolvers after sorting (0 means all)")
	fs.DurationVar(&c.maxDuration, "max-duration", 0, "Maximum duration for the entire run (0 disables)")
	fs.StringVar(&c.validation, "validation", "baseline", "Correctness validation: baseline, expected or off")
	fs.IntVar(&c.validationRetries, "validation-retries", 1, "Extra attempts for validation timeout/transport failures (0-3)")
	fs.StringSliceVar(&c.recordTypes, "record-types", []string{"A"}, "Correctness record types: A, AAAA or A,AAAA; measurements remain A")
	fs.StringSliceVar(&c.baselineInputs, "baseline", []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}, "Trusted reference resolvers (repeatable or comma-separated)")
	fs.IntVar(&c.quorum, "baseline-quorum", 0, "Required matching references (0 selects a strict majority)")
	fs.StringSliceVar(&c.positiveDomains, "positive-domain", nil, "Additional positive-validation domains (repeatable)")
	fs.StringSliceVar(&c.negativeDomains, "negative-domain", nil, "Negative-check domains; replaces defaults, root is always included")
	fs.StringArrayVar(&c.expectedInputs, "expect", nil, "Expected addresses: domain=IP,IP (repeatable; family selects A or AAAA)")
	fs.StringVar(&c.queryPrefix, "query-prefix", "", "Optional prefix for random DNS query labels")
	fs.BoolVar(&c.includeFiltered, "include-filtered", false, "Include rejected resolvers in CSV or JSON exports")
	fs.BoolVar(&saveDNS, "save-dns", true, "Legacy output selector; false writes headerless CSV")
	fs.StringVar(&c.format, "format", "", "Output format: dns, csv or json (default dns)")
	fs.StringVar(&c.sortBy, "sort", "latency", "Sort by latency, p95, rate or input")
	fs.BoolVar(&c.showVersion, "version", false, "Print version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.showVersion {
		return c, nil
	}
	if !fs.Changed("precheck-tests") && c.validation != "off" {
		c.prechecks = 0
	}
	if fs.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if c.input == "" && len(c.resolverInputs) == 0 {
		return c, errors.New("--in or --resolver is required (use --in - for stdin)")
	}
	if c.workers < 1 || c.workers > 251 {
		return c, errors.New("--workers must be between 1 and 251")
	}
	if c.tests < 1 || c.tests > 5000 {
		return c, errors.New("--tests must be between 1 and 5000")
	}
	if c.timeout <= 0 {
		return c, errors.New("--timeout must be positive")
	}
	if c.top < 0 || c.maxDuration < 0 {
		return c, errors.New("--top and --max-duration cannot be negative")
	}
	if !finite(c.qps) || c.qps <= 0 || c.qps > 1000000 {
		return c, errors.New("--qps must be positive and at most 1000000")
	}
	if c.prechecks < 0 || c.prechecks > 1000 || c.precheckErrors < 0 {
		return c, errors.New("invalid precheck limits")
	}
	if !finite(c.maxTime) || c.maxTime < 0 || !finite(c.maxP95) || c.maxP95 < 0 || c.maxErrors < 0 || !finite(c.minRate) || c.minRate < 0 || c.minRate > 100 {
		return c, errors.New("invalid filter thresholds")
	}
	c.domain = dns.Fqdn(strings.ToLower(strings.TrimSpace(c.domain)))
	if !validHostname(strings.TrimSuffix(c.domain, ".")) || len(c.domain) > 237 {
		return c, errors.New("--domain must be a valid hostname with room for a random label")
	}
	if c.format == "" {
		c.format = "dns"
		if !saveDNS {
			c.format = "legacy-csv"
		}
	} else if fs.Changed("save-dns") {
		return c, errors.New("use either --format or --save-dns")
	}
	switch c.format {
	case "dns", "csv", "json", "legacy-csv":
	default:
		return c, errors.New("--format must be dns, csv or json")
	}
	if c.includeFiltered && (c.format == "dns" || c.format == "legacy-csv") {
		return c, errors.New("--include-filtered requires --format csv or json")
	}
	switch c.sortBy {
	case "latency", "p95", "rate", "input":
	default:
		return c, errors.New("--sort must be latency, p95, rate or input")
	}
	if !fs.Changed("negative-domain") {
		c.negativeDomains = []string{"facebook.com", "paypal.com", "google.com", "bet365.com", "wikileaks.com"}
	}
	if err := configureValidation(&c); err != nil {
		return c, err
	}
	return c, nil
}

func buildVersion(info *debug.BuildInfo) string {
	if version != "dev" {
		return version
	}
	if info != nil {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		revision, dirty := "", false
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" {
				dirty = setting.Value == "true"
			}
		}
		if revision != "" {
			if dirty {
				revision += "+dirty"
			}
			return revision
		}
	}
	return "dev"
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

type endpoint struct{ address, label string }

func parseEndpoint(value string) (endpoint, error) {
	host, port := value, "53"
	if ip, err := netip.ParseAddr(value); err == nil {
		host = ip.Unmap().String()
	} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		ip, err := netip.ParseAddr(value[1 : len(value)-1])
		if err != nil {
			return endpoint{}, errors.New("invalid bracketed IP address")
		}
		host = ip.Unmap().String()
	} else if strings.Contains(value, ":") {
		var err error
		host, port, err = net.SplitHostPort(value)
		if err != nil {
			return endpoint{}, err
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return endpoint{}, errors.New("port must be between 1 and 65535")
	}
	port = strconv.Itoa(p)
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if !validHostname(host) {
			return endpoint{}, errors.New("invalid resolver hostname or IP address")
		}
	}
	address := net.JoinHostPort(host, port)
	label := address
	if port == "53" {
		label = host
	}
	return endpoint{address: address, label: label}, nil
}

func readResolvers(r io.Reader) ([]endpoint, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	seen := make(map[string]bool)
	var servers []endpoint
	line := 0
	for scanner.Scan() {
		line++
		value := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(scanner.Text(), "\ufeff"), "#", 2)[0])
		if value == "" {
			continue
		}
		server, err := parseEndpoint(value)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if !seen[server.address] {
			seen[server.address] = true
			servers = append(servers, server)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read resolvers: %w", err)
	}
	if len(servers) == 0 {
		return nil, errors.New("resolver list is empty")
	}
	return servers, nil
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c, err := parseConfig(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "dnsfaster:", err)
		return 2
	}
	if c.showVersion {
		info, _ := debug.ReadBuildInfo()
		if _, err := fmt.Fprintln(stdout, buildVersion(info)); err != nil {
			return failRun(stderr, err)
		}
		return 0
	}
	started := time.Now()
	if c.maxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.maxDuration)
		defer cancel()
	}
	servers, excluded, err := loadResolvers(ctx, c, stdin)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return failRun(stderr, err)
	}
	// Reserve the temporary output before generating network traffic.
	var output *outputFile
	if c.output != "" && c.output != "-" {
		if c.excludeFile != "" && !isRemoteSource(c.excludeFile) {
			a, ea := os.Stat(c.excludeFile)
			b, eb := os.Stat(c.output)
			if ea == nil && eb == nil && os.SameFile(a, b) {
				return failRun(stderr, errors.New("output must not overwrite the exclusion file"))
			}
		}
		inputPath := c.input
		if inputPath == "" || isRemoteSource(inputPath) {
			inputPath = "-"
		}
		output, err = prepareOutput(c.output, inputPath)
		if err != nil {
			return failRun(stderr, err)
		}
		defer output.abort()
	}
	var progress *progressReporter
	if !c.quiet {
		progress = &progressReporter{writer: stderr, started: started, enabled: c.progress, verbose: c.verbose, total: len(servers)}
		if err := progress.phase("starting " + c.validation + " validation"); err != nil {
			return failRun(stderr, err)
		}
	}
	results, err := measureWithProgress(ctx, c, servers, progress)
	if err != nil {
		return failRun(stderr, err)
	}
	sortResults(results, c.sortBy)
	if !c.quiet {
		if err := writeReport(stderr, results); err != nil {
			return failRun(stderr, err)
		}
		if err := writeSummary(stderr, results, excluded, c.top, started); err != nil {
			return failRun(stderr, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return failRun(stderr, err)
	}
	if c.output != "" {
		var dest io.Writer = stdout
		if output != nil {
			dest = output.file
		}
		if err := writeResults(dest, selectExports(results, c.top, c.includeFiltered), c.format, c.includeFiltered); err != nil {
			return failRun(stderr, err)
		}
		if output != nil {
			if err := ctx.Err(); err != nil {
				return failRun(stderr, err)
			}
			if err := output.commit(); err != nil {
				return failRun(stderr, err)
			}
		}
	}
	for _, r := range results {
		if !r.Filtered {
			return 0
		}
	}
	return 1
}

func failRun(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "dnsfaster:", err)
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
