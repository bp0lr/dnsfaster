package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/miekg/dns"
	"golang.org/x/time/rate"
)

type positiveCheck struct {
	domain    string
	addresses []string
}
type validationPlan struct {
	positives []positiveCheck
	negatives []string
}

func configureValidation(c *config) error {
	switch c.validation {
	case "baseline", "expected", "off":
	default:
		return errors.New("--validation must be baseline, expected or off")
	}
	c.queryPrefix = strings.ToLower(strings.TrimSpace(c.queryPrefix))
	if c.queryPrefix != "" && (strings.Contains(c.queryPrefix, ".") || !validHostname(c.queryPrefix) || len(c.queryPrefix) > 46) {
		return errors.New("--query-prefix must be a DNS label of at most 46 characters")
	}
	positive, err := normalizeDomains(append([]string{c.domain}, c.positiveDomains...), 0)
	if err != nil {
		return fmt.Errorf("positive domain: %w", err)
	}
	c.positiveDomains = positive
	labelLength := 16
	if c.queryPrefix != "" {
		labelLength += len(c.queryPrefix) + 1
	}
	negative, err := normalizeDomains(append([]string{c.domain}, c.negativeDomains...), labelLength)
	if err != nil {
		return fmt.Errorf("negative domain: %w", err)
	}
	c.negativeDomains = negative
	expected, err := expectedAnswers(c.expectedInputs)
	if err != nil {
		return err
	}
	for name := range expected {
		if !slices.Contains(c.positiveDomains, name) {
			return fmt.Errorf("--expect domain %s must be --domain or a --positive-domain", name)
		}
	}
	if c.validation == "off" && len(expected) > 0 {
		return errors.New("--expect cannot be used with --validation off")
	}
	if c.validation == "expected" {
		for _, name := range c.positiveDomains {
			if len(expected[name]) == 0 {
				return fmt.Errorf("--validation expected requires --expect for %s", name)
			}
		}
	}
	c.baselines = nil
	seen := make(map[string]bool)
	for _, value := range c.baselineInputs {
		ep, err := parseEndpoint(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("--baseline: %w", err)
		}
		if !seen[ep.address] {
			seen[ep.address] = true
			c.baselines = append(c.baselines, ep)
		}
	}
	if len(c.baselines) > 32 {
		return errors.New("at most 32 reference resolvers are supported")
	}
	if c.quorum < 0 {
		return errors.New("--baseline-quorum cannot be negative")
	}
	if c.validation == "baseline" {
		if len(c.baselines) == 0 {
			return errors.New("baseline validation requires reference resolvers")
		}
		if c.quorum == 0 {
			c.quorum = len(c.baselines)/2 + 1
		}
		if c.quorum <= len(c.baselines)/2 || c.quorum > len(c.baselines) {
			return errors.New("--baseline-quorum must be a strict majority of distinct references")
		}
	}
	return nil
}

func normalizeDomains(values []string, randomLabelLength int) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for _, value := range values {
		name := dns.Fqdn(strings.ToLower(strings.TrimSpace(value)))
		if !validHostname(strings.TrimSuffix(name, ".")) || len(name) > 254 || (randomLabelLength > 0 && len(name)+randomLabelLength+1 > 254) {
			return nil, fmt.Errorf("invalid or oversized domain %q", value)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

func expectedAnswers(values []string) (map[string][]string, error) {
	expected := make(map[string][]string)
	for _, value := range values {
		name, addresses, ok := strings.Cut(value, "=")
		if !ok {
			return nil, errors.New("--expect must be domain=IPv4,IPv4")
		}
		names, err := normalizeDomains([]string{name}, 0)
		if err != nil {
			return nil, err
		}
		for _, address := range strings.Split(addresses, ",") {
			ip, err := netip.ParseAddr(strings.TrimSpace(address))
			if err != nil || !ip.Unmap().Is4() {
				return nil, fmt.Errorf("--expect requires IPv4 A answers: %q", address)
			}
			expected[names[0]] = append(expected[names[0]], ip.Unmap().String())
		}
	}
	for name, answers := range expected {
		sort.Strings(answers)
		expected[name] = slices.Compact(answers)
	}
	return expected, nil
}

func randomName(domain, prefix string) string {
	if prefix != "" {
		return fmt.Sprintf("%s-%016x.%s", prefix, rand.Uint64(), domain)
	}
	return fmt.Sprintf("%016x.%s", rand.Uint64(), domain)
}

// Compare only addresses belonging to the question or its CNAME chain. TTL,
// answer ordering and unrelated records do not change the expected answer set.
func answerAddresses(reply *dns.Msg, name string) ([]string, string) {
	aliases := make(map[string]string)
	addresses := make(map[string][]string)
	for _, record := range reply.Answer {
		if record.Header().Class != dns.ClassINET {
			continue
		}
		owner := dns.CanonicalName(record.Header().Name)
		switch rr := record.(type) {
		case *dns.CNAME:
			target := dns.CanonicalName(rr.Target)
			if previous, ok := aliases[owner]; ok && previous != target {
				return nil, "invalid_cname"
			}
			aliases[owner] = target
		case *dns.A:
			ip, ok := netip.AddrFromSlice(rr.A)
			if !ok || !ip.Unmap().Is4() {
				return nil, "invalid_address"
			}
			addresses[owner] = append(addresses[owner], ip.Unmap().String())
		}
	}
	owner := dns.CanonicalName(name)
	seen := make(map[string]bool)
	for {
		if seen[owner] {
			return nil, "cname_loop"
		}
		seen[owner] = true
		target, alias := aliases[owner]
		if alias {
			if len(addresses[owner]) > 0 {
				return nil, "invalid_cname"
			}
			owner = target
			continue
		}
		answers := addresses[owner]
		if len(answers) == 0 {
			return nil, "no_address_answers"
		}
		sort.Strings(answers)
		return slices.Compact(answers), ""
	}
}

func newQueryClient(c config, limiter *rate.Limiter) *queryClient {
	return &queryClient{udp: dns.Client{Net: "udp", Timeout: c.timeout}, tcp: dns.Client{Net: "tcp", Timeout: c.timeout}, limiter: limiter, fallback: c.tcpFallback, timeout: c.timeout}
}

type referenceQuestion struct {
	name     string
	positive bool
}
type referenceResult struct{ keys, reasons []string }

func prepareValidation(ctx context.Context, c config, limiter *rate.Limiter) (validationPlan, error) {
	plan := validationPlan{}
	if c.validation == "off" {
		return plan, nil
	}
	expected, err := expectedAnswers(c.expectedInputs)
	if err != nil {
		return plan, err
	}
	var questions []referenceQuestion
	for _, name := range c.positiveDomains {
		plan.positives = append(plan.positives, positiveCheck{domain: name, addresses: expected[name]})
		if len(expected[name]) == 0 {
			questions = append(questions, referenceQuestion{name: name, positive: true})
		}
	}
	for _, domain := range c.negativeDomains {
		name := randomName(domain, c.queryPrefix)
		plan.negatives = append(plan.negatives, name)
		if c.validation == "baseline" {
			questions = append(questions, referenceQuestion{name: name})
		}
	}
	if c.validation == "expected" {
		return plan, nil
	}
	results := make(chan referenceResult, len(c.baselines))
	for _, reference := range c.baselines {
		go func() {
			result := referenceResult{keys: make([]string, len(questions)), reasons: make([]string, len(questions))}
			client := newQueryClient(c, limiter)
			var msg dns.Msg
			for i, q := range questions {
				if ctx.Err() != nil {
					result.reasons[i] = "canceled"
					continue
				}
				msg.SetQuestion(q.name, dns.TypeA)
				code := dns.RcodeNameError
				if q.positive {
					code = dns.RcodeSuccess
				}
				reply, _, reason := client.lookup(ctx, &msg, reference.address, code)
				if reason == "" {
					if q.positive {
						var answers []string
						answers, reason = answerAddresses(reply, q.name)
						result.keys[i] = strings.Join(answers, ",")
					} else {
						result.keys[i] = "NXDOMAIN"
					}
				}
				result.reasons[i] = reason
			}
			results <- result
		}()
	}
	votes := make([]map[string]int, len(questions))
	for i := range votes {
		votes[i] = make(map[string]int)
	}
	for range c.baselines {
		result := <-results
		for i, key := range result.keys {
			if result.reasons[i] == "" {
				votes[i][key]++
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	for i, q := range questions {
		consensus := ""
		for key, count := range votes[i] {
			if count >= c.quorum {
				consensus = key
				break
			}
		}
		if consensus == "" {
			return plan, fmt.Errorf("no reference quorum for %s: need %d of %d matching responses; choose stable domains or explicit expected answers", q.name, c.quorum, len(c.baselines))
		}
		if q.positive {
			for j := range plan.positives {
				if plan.positives[j].domain == q.name {
					plan.positives[j].addresses = strings.Split(consensus, ",")
					break
				}
			}
		}
	}
	return plan, nil
}

func validateResolver(ctx context.Context, plan validationPlan, server endpoint, client *queryClient, r *resultStats) {
	var msg dns.Msg
	reject := func(reason, name string) {
		r.ValidationFailures++
		r.Errors["validation_"+reason]++
		r.Reasons = append(r.Reasons, reason+":"+name)
		r.Filtered = true
	}
	for _, check := range plan.positives {
		if ctx.Err() != nil {
			return
		}
		msg.SetQuestion(check.domain, dns.TypeA)
		reply, _, reason := client.lookup(ctx, &msg, server.address, dns.RcodeSuccess)
		r.ValidationChecks++
		if reason == "" {
			var addresses []string
			addresses, reason = answerAddresses(reply, check.domain)
			if reason == "" && !slices.Equal(addresses, check.addresses) {
				reason = "positive_mismatch"
			}
		}
		if reason != "" {
			reject(reason, check.domain)
			return
		}
	}
	for _, name := range plan.negatives {
		if ctx.Err() != nil {
			return
		}
		msg.SetQuestion(name, dns.TypeA)
		_, reason := client.query(ctx, &msg, server.address, dns.RcodeNameError)
		r.ValidationChecks++
		if reason != "" {
			reject("negative_"+reason, name)
			return
		}
	}
}
