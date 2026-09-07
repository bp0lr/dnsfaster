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
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	var c config
	var saveDNS, showVersion bool
	fs := flag.NewFlagSet("dnsfaster", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.input, "in", "", "Resolver file, or - for stdin (required)")
	fs.StringVar(&c.output, "out", "", "Output file, or - for stdout")
	fs.StringVar(&c.domain, "domain", "example.com", "Base domain without wildcard DNS")
	fs.IntVar(&c.workers, "workers", 10, "Concurrent resolver checks (1-251)")
	fs.IntVar(&c.tests, "tests", 10, "Measurements per resolver (1-5000)")
	fs.DurationVar(&c.timeout, "timeout", 2*time.Second, "Timeout per query, including TCP fallback")
	fs.Float64Var(&c.qps, "qps", 50, "Global query rate limit, including prechecks and TCP retries")
	fs.IntVar(&c.prechecks, "precheck-tests", 3, "Base-domain checks per resolver (0 disables, maximum 1000)")
	fs.IntVar(&c.precheckErrors, "precheck-errors", 1, "Allowed failures during prechecks")
	fs.Float64Var(&c.maxTime, "filter-time", 0, "Maximum mean latency in milliseconds (0 disables)")
	fs.IntVar(&c.maxErrors, "filter-errors", 0, "Maximum measurement failures (0 disables)")
	fs.Float64Var(&c.minRate, "filter-rate", 0, "Minimum success percentage (0 disables)")
	fs.BoolVar(&c.tcpFallback, "tcp-fallback", false, "Retry truncated UDP responses over TCP")
	fs.BoolVar(&c.quiet, "quiet", false, "Suppress the console report")
	fs.BoolVar(&c.includeFiltered, "include-filtered", false, "Include rejected resolvers in CSV or JSON exports")
	fs.BoolVar(&saveDNS, "save-dns", true, "Legacy output selector; false writes headerless CSV")
	fs.StringVar(&c.format, "format", "", "Output format: dns, csv or json (default dns)")
	fs.StringVar(&c.sortBy, "sort", "latency", "Sort by latency, rate or input")
	fs.BoolVar(&showVersion, "version", false, "Print version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if showVersion {
		fmt.Fprintln(stderr, version)
		return c, flag.ErrHelp
	}
	if fs.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if c.input == "" {
		return c, errors.New("--in is required (use --in - for stdin)")
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
	if !finite(c.qps) || c.qps <= 0 || c.qps > 1000000 {
		return c, errors.New("--qps must be positive and at most 1000000")
	}
	if c.prechecks < 0 || c.prechecks > 1000 || c.precheckErrors < 0 {
		return c, errors.New("invalid precheck limits")
	}
	if !finite(c.maxTime) || c.maxTime < 0 || c.maxErrors < 0 || !finite(c.minRate) || c.minRate < 0 || c.minRate > 100 {
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
	case "latency", "rate", "input":
	default:
		return c, errors.New("--sort must be latency, rate or input")
	}
	return c, nil
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
	input := stdin
	if c.input != "-" {
		f, err := os.Open(c.input)
		if err != nil {
			fmt.Fprintln(stderr, "dnsfaster:", err)
			return 1
		}
		defer f.Close()
		input = f
	}
	servers, err := readResolvers(input)
	if err != nil {
		fmt.Fprintln(stderr, "dnsfaster:", err)
		return 1
	}
	// Reserve the temporary output before generating network traffic.
	var output *outputFile
	if c.output != "" && c.output != "-" {
		output, err = prepareOutput(c.output, c.input)
		if err != nil {
			fmt.Fprintln(stderr, "dnsfaster:", err)
			return 1
		}
		defer output.abort()
	}
	results, err := measure(ctx, c, servers)
	if err != nil {
		fmt.Fprintln(stderr, "dnsfaster:", err)
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	sortResults(results, c.sortBy)
	if !c.quiet {
		if err := writeReport(stderr, results); err != nil {
			fmt.Fprintln(stderr, "dnsfaster:", err)
			return 1
		}
	}
	if c.output != "" {
		var dest io.Writer = stdout
		if output != nil {
			dest = output.file
		}
		if err := writeResults(dest, results, c.format, c.includeFiltered); err != nil {
			fmt.Fprintln(stderr, "dnsfaster:", err)
			return 1
		}
		if output != nil {
			if err := output.commit(); err != nil {
				fmt.Fprintln(stderr, "dnsfaster:", err)
				return 1
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
