package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/time/rate"
)

type resultStats struct {
	Resolver           string         `json:"resolver"`
	AverageMS          float64        `json:"average_ms"`
	P50MS              float64        `json:"p50_ms"`
	P95MS              float64        `json:"p95_ms"`
	SuccessRate        float64        `json:"success_percent"`
	Successes          int            `json:"successes"`
	Failures           int            `json:"failures"`
	PrecheckFailures   int            `json:"precheck_failures"`
	ValidationChecks   int            `json:"validation_checks"`
	ValidationFailures int            `json:"validation_failures"`
	Filtered           bool           `json:"filtered"`
	Reasons            []string       `json:"reasons"`
	Errors             map[string]int `json:"errors"`
	index              int
}

type queryClient struct {
	udp, tcp dns.Client
	limiter  *rate.Limiter
	fallback bool
	timeout  time.Duration
}

// Closing the connection on cancellation also interrupts an in-flight read.
func exchange(ctx context.Context, client *dns.Client, msg *dns.Msg, address string) (*dns.Msg, error) {
	conn, err := client.DialContext(ctx, address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reply, _, err := client.ExchangeWithConnContext(ctx, msg, conn)
	return reply, err
}

func (c *queryClient) query(ctx context.Context, msg *dns.Msg, address string, expected int) (float64, string) {
	_, elapsed, reason := c.lookup(ctx, msg, address, expected)
	return elapsed, reason
}

func (c *queryClient) lookup(ctx context.Context, msg *dns.Msg, address string, expected int) (*dns.Msg, float64, string) {
	if err := c.limiter.Wait(ctx); err != nil {
		// Wait may reject a reservation before the parent deadline expires.
		// Do not turn unscheduled queries into completed failed samples.
		if _, bounded := ctx.Deadline(); bounded && ctx.Err() == nil {
			<-ctx.Done()
		}
		return nil, 0, "canceled"
	}
	queryCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	start := time.Now()
	reply, err := exchange(queryCtx, &c.udp, msg, address)
	if err == nil && reply != nil && reply.Truncated && c.fallback {
		if err = c.limiter.Wait(queryCtx); err != nil {
			if ctx.Err() != nil {
				return nil, 0, "canceled"
			}
			return nil, 0, "timeout"
		}
		reply, err = exchange(queryCtx, &c.tcp, msg, address)
	}
	elapsed := float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil {
		var networkErr net.Error
		switch {
		case ctx.Err() != nil:
			return nil, 0, "canceled"
		case queryCtx.Err() != nil || (errors.As(err, &networkErr) && networkErr.Timeout()):
			return nil, 0, "timeout"
		default:
			return nil, 0, "transport"
		}
	}
	if reply == nil {
		return nil, 0, "empty_response"
	}
	if !reply.Response || reply.Opcode != msg.Opcode || len(reply.Question) != 1 || len(msg.Question) != 1 || !strings.EqualFold(reply.Question[0].Name, msg.Question[0].Name) || reply.Question[0].Qtype != msg.Question[0].Qtype || reply.Question[0].Qclass != msg.Question[0].Qclass {
		return nil, 0, "invalid_response"
	}
	if reply.Truncated {
		return nil, 0, "truncated"
	}
	if reply.Rcode != expected {
		name, ok := dns.RcodeToString[reply.Rcode]
		if !ok {
			name = fmt.Sprint(reply.Rcode)
		}
		return nil, 0, "rcode_" + name
	}
	if expected == dns.RcodeNameError && len(reply.Answer) != 0 {
		return nil, 0, "unexpected_answers"
	}
	return reply, elapsed, ""
}

func checkResolver(ctx context.Context, c config, server endpoint, names []string, client *queryClient, samples []float64, plan validationPlan) resultStats {
	r := resultStats{Resolver: server.label, Reasons: []string{}, Errors: make(map[string]int)}
	validateResolver(ctx, plan, server, client, &r)
	if r.Filtered || ctx.Err() != nil {
		return r
	}
	var msg dns.Msg
	msg.SetQuestion(c.domain, dns.TypeA)
	precheckSuccesses := 0
	for range c.prechecks {
		if ctx.Err() != nil {
			return r
		}
		_, reason := client.query(ctx, &msg, server.address, dns.RcodeSuccess)
		if reason != "" {
			r.PrecheckFailures++
			r.Errors["precheck_"+reason]++
			if r.PrecheckFailures > c.precheckErrors {
				break
			}
		} else {
			precheckSuccesses++
		}
	}
	if c.prechecks > 0 && (r.PrecheckFailures > c.precheckErrors || precheckSuccesses == 0) {
		r.Filtered = true
		r.Reasons = append(r.Reasons, "precheck_failed")
		return r
	}
	samples = samples[:0]
	sum := 0.0
	for _, name := range names {
		if ctx.Err() != nil {
			return r
		}
		msg.SetQuestion(name, dns.TypeA)
		elapsed, reason := client.query(ctx, &msg, server.address, dns.RcodeNameError)
		if reason != "" {
			r.Failures++
			r.Errors[reason]++
		} else {
			r.Successes++
			sum += elapsed
			samples = append(samples, elapsed)
		}
	}
	if r.Successes > 0 {
		r.AverageMS = sum / float64(r.Successes)
		sort.Float64s(samples)
		r.P50MS = percentile(samples, 0.50)
		r.P95MS = percentile(samples, 0.95)
	}
	r.SuccessRate = 100 * float64(r.Successes) / float64(c.tests)
	if r.Successes == 0 {
		r.Reasons = append(r.Reasons, "no_successful_queries")
	}
	if c.maxTime > 0 && r.AverageMS > c.maxTime {
		r.Reasons = append(r.Reasons, "latency")
	}
	if c.maxP95 > 0 && r.P95MS > c.maxP95 {
		r.Reasons = append(r.Reasons, "p95_latency")
	}
	if c.maxErrors > 0 && r.Failures > c.maxErrors {
		r.Reasons = append(r.Reasons, "errors")
	}
	if c.minRate > 0 && r.SuccessRate < c.minRate {
		r.Reasons = append(r.Reasons, "success_rate")
	}
	r.Filtered = len(r.Reasons) != 0
	return r
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Ceil(p*float64(len(sorted))))-1]
}

func measure(ctx context.Context, c config, servers []endpoint) ([]resultStats, error) {
	return measureWithProgress(ctx, c, servers, nil)
}

func measureWithProgress(ctx context.Context, c config, servers []endpoint, progress *progressReporter) ([]resultStats, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// All resolvers receive the same names. Only the selected base domain is tested.
	names := make([]string, c.tests)
	for i := range names {
		names[i] = randomName(c.domain, c.queryPrefix)
	}
	workers := min(c.workers, len(servers))
	limiter := rate.NewLimiter(rate.Limit(c.qps), 1)
	plan, err := prepareValidation(ctx, c, limiter)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		if err := progress.phase("checking candidates"); err != nil {
			return nil, err
		}
	}
	jobs := make(chan int)
	results := make(chan resultStats, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := newQueryClient(c, limiter)
			samples := make([]float64, 0, c.tests)
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				r := checkResolver(ctx, c, servers[index], names, client, samples, plan)
				r.index = index
				select {
				case results <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range servers {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	collected := make([]resultStats, 0, len(servers))
	var ticks <-chan time.Time
	if progress != nil && progress.enabled {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	var reportErr error
collect:
	for {
		select {
		case r, ok := <-results:
			if !ok {
				break collect
			}
			collected = append(collected, r)
			if progress != nil && reportErr == nil {
				reportErr = progress.complete(r)
			}
		case <-ticks:
			if reportErr == nil {
				reportErr = progress.tick()
			}
		}
		if reportErr != nil {
			cancel()
		}
	}
	if reportErr != nil {
		return nil, reportErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return collected, nil
}

func sortResults(results []resultStats, by string) {
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if by == "input" {
			return a.index < b.index
		}
		if a.Filtered != b.Filtered {
			return !a.Filtered
		}
		if (a.Successes == 0) != (b.Successes == 0) {
			return a.Successes > 0
		}
		if by == "rate" && a.SuccessRate != b.SuccessRate {
			return a.SuccessRate > b.SuccessRate
		}
		if by == "p95" && a.P95MS != b.P95MS {
			return a.P95MS < b.P95MS
		}
		if a.AverageMS != b.AverageMS {
			return a.AverageMS < b.AverageMS
		}
		if a.SuccessRate != b.SuccessRate {
			return a.SuccessRate > b.SuccessRate
		}
		return a.index < b.index
	})
}
