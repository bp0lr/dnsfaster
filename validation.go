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
	"sync"

	"github.com/miekg/dns"
	"golang.org/x/time/rate"
)

type positiveCheck struct {
	domain    string
	qtype     uint16
	addresses []string
}
type validationPlan struct {
	positives []positiveCheck
	negatives []string
	qtypes    []uint16
}

func configureValidation(c *config) error {
	if c.validationRetries < 0 || c.validationRetries > 3 {
		return errors.New("--validation-retries must be between 0 and 3")
	}
	if len(c.recordTypes) == 0 {
		return errors.New("--record-types requires A, AAAA or both")
	}
	for i, value := range c.recordTypes {
		c.recordTypes[i] = strings.ToUpper(strings.TrimSpace(value))
		if c.recordTypes[i] != "A" && c.recordTypes[i] != "AAAA" {
			return errors.New("--record-types supports only A and AAAA")
		}
	}
	sort.Strings(c.recordTypes)
	c.recordTypes = slices.Compact(c.recordTypes)
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
	for name, addresses := range expected {
		if !slices.Contains(c.positiveDomains, name) {
			return fmt.Errorf("--expect domain %s must be --domain or a --positive-domain", name)
		}
		for _, address := range addresses {
			kind := "AAAA"
			if netip.MustParseAddr(address).Is4() {
				kind = "A"
			}
			if !slices.Contains(c.recordTypes, kind) {
				return fmt.Errorf("--expect %s requires --record-types to include %s", address, kind)
			}
		}
	}
	if c.validation == "off" && len(expected) > 0 {
		return errors.New("--expect cannot be used with --validation off")
	}
	if c.validation == "expected" {
		for _, name := range c.positiveDomains {
			for _, kind := range c.recordTypes {
				if len(addressesOfType(expected[name], dns.StringToType[kind])) == 0 {
					return fmt.Errorf("--validation expected requires --expect %s answers for %s", kind, name)
				}
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
			return nil, errors.New("--expect must be domain=IP,IP")
		}
		names, err := normalizeDomains([]string{name}, 0)
		if err != nil {
			return nil, err
		}
		for _, address := range strings.Split(addresses, ",") {
			ip, err := netip.ParseAddr(strings.TrimSpace(address))
			if err != nil || ip.Zone() != "" {
				return nil, fmt.Errorf("--expect requires IPv4 or IPv6 addresses without a zone: %q", address)
			}
			expected[names[0]] = append(expected[names[0]], ip.String())
		}
	}
	for name, answers := range expected {
		sort.Strings(answers)
		expected[name] = slices.Compact(answers)
	}
	return expected, nil
}

func addressesOfType(addresses []string, qtype uint16) []string {
	var selected []string
	for _, address := range addresses {
		if netip.MustParseAddr(address).Is4() == (qtype == dns.TypeA) {
			selected = append(selected, address)
		}
	}
	return selected
}

func randomName(domain, prefix string) string {
	if prefix != "" {
		return fmt.Sprintf("%s-%016x.%s", prefix, rand.Uint64(), domain)
	}
	return fmt.Sprintf("%016x.%s", rand.Uint64(), domain)
}

// Compare only addresses belonging to the question or its CNAME chain. TTL,
// answer ordering and unrelated records do not change the expected answer set.
func answerAddresses(reply *dns.Msg, name string, qtype uint16) ([]string, string) {
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
			if qtype != dns.TypeA {
				continue
			}
			ip, ok := netip.AddrFromSlice(rr.A)
			if !ok || !ip.Unmap().Is4() {
				return nil, "invalid_address"
			}
			addresses[owner] = append(addresses[owner], ip.Unmap().String())
		case *dns.AAAA:
			if qtype != dns.TypeAAAA {
				continue
			}
			ip, ok := netip.AddrFromSlice(rr.AAAA)
			if !ok || !ip.Is6() {
				return nil, "invalid_address"
			}
			addresses[owner] = append(addresses[owner], ip.String())
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
	return &queryClient{udp: dns.Client{Net: "udp", Timeout: c.timeout}, tcp: dns.Client{Net: "tcp", Timeout: c.timeout}, limiter: limiter, fallback: c.tcpFallback, timeout: c.timeout, validationRetries: c.validationRetries}
}

// Retries are confined to correctness checks. Each attempt uses the shared
// limiter and a fresh query timeout; the caller's overall deadline still applies.
func (c *queryClient) validationLookup(ctx context.Context, msg *dns.Msg, address string, expected int) (*dns.Msg, string, int) {
	for attempt := 0; ; attempt++ {
		reply, _, reason := c.lookup(ctx, msg, address, expected)
		if ctx.Err() != nil || attempt >= c.validationRetries || (reason != "timeout" && reason != "transport") {
			return reply, reason, attempt
		}
		msg.Id = dns.Id()
	}
}

type referenceQuestion struct {
	name     string
	qtype    uint16
	positive bool
}
type referenceResult struct {
	question    int
	key, reason string
}

func prepareValidation(ctx context.Context, c config, limiter *rate.Limiter) (validationPlan, error) {
	plan := validationPlan{}
	if c.validation == "off" {
		return plan, nil
	}
	for _, kind := range c.recordTypes {
		plan.qtypes = append(plan.qtypes, dns.StringToType[kind])
	}
	expected, err := expectedAnswers(c.expectedInputs)
	if err != nil {
		return plan, err
	}
	var questions []referenceQuestion
	for _, name := range c.positiveDomains {
		for _, qtype := range plan.qtypes {
			addresses := addressesOfType(expected[name], qtype)
			plan.positives = append(plan.positives, positiveCheck{domain: name, qtype: qtype, addresses: addresses})
			if len(addresses) == 0 {
				questions = append(questions, referenceQuestion{name: name, qtype: qtype, positive: true})
			}
		}
	}
	for _, domain := range c.negativeDomains {
		name := randomName(domain, c.queryPrefix)
		plan.negatives = append(plan.negatives, name)
		if c.validation == "baseline" {
			for _, qtype := range plan.qtypes {
				questions = append(questions, referenceQuestion{name: name, qtype: qtype})
			}
		}
	}
	if c.validation == "expected" {
		return plan, nil
	}
	// Cancel only the reference phase; candidate workers keep the parent context.
	refCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan referenceResult, len(c.baselines))
	var wg sync.WaitGroup
	for _, reference := range c.baselines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := newQueryClient(c, limiter)
			var msg dns.Msg
			for i, q := range questions {
				if refCtx.Err() != nil {
					return
				}
				msg.SetQuestion(q.name, q.qtype)
				code := dns.RcodeNameError
				if q.positive {
					code = dns.RcodeSuccess
				}
				reply, reason, _ := client.validationLookup(refCtx, &msg, reference.address, code)
				key := "NXDOMAIN"
				if reason == "" && q.positive {
					var answers []string
					answers, reason = answerAddresses(reply, q.name, q.qtype)
					key = strings.Join(answers, ",")
				}
				select {
				case results <- referenceResult{question: i, key: key, reason: reason}:
				case <-refCtx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	votes := make([]map[string]int, len(questions))
	received := make([]int, len(questions))
	consensus := make([]string, len(questions))
	for i := range votes {
		votes[i] = make(map[string]int)
	}
	resolved := 0
	var quorumErr error
	for result := range results {
		i := result.question
		if quorumErr != nil || consensus[i] != "" {
			continue
		}
		received[i]++
		if result.reason == "" {
			votes[i][result.key]++
			if votes[i][result.key] >= c.quorum {
				consensus[i] = result.key
				resolved++
				if resolved == len(questions) {
					cancel()
				}
				continue
			}
		}
		best := 0
		for _, count := range votes[i] {
			best = max(best, count)
		}
		if best+len(c.baselines)-received[i] < c.quorum {
			q := questions[i]
			quorumErr = fmt.Errorf("no reference quorum for %s/%s: need %d of %d matching responses; choose stable domains or explicit expected answers", q.name, dns.TypeToString[q.qtype], c.quorum, len(c.baselines))
			cancel()
		}
	}
	// Draining the channel joins all reference workers before candidates start.
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if quorumErr != nil {
		return plan, quorumErr
	}
	for i, q := range questions {
		if q.positive {
			for j := range plan.positives {
				if plan.positives[j].domain == q.name && plan.positives[j].qtype == q.qtype {
					plan.positives[j].addresses = strings.Split(consensus[i], ",")
					break
				}
			}
		}
	}
	return plan, nil
}

func validateResolver(ctx context.Context, plan validationPlan, server endpoint, client *queryClient, r *resultStats) {
	var msg dns.Msg
	reject := func(reason, name string, qtype uint16) {
		r.ValidationFailures++
		r.Errors["validation_"+reason]++
		r.Reasons = append(r.Reasons, reason+":"+name+"/"+dns.TypeToString[qtype])
		r.Filtered = true
	}
	for _, check := range plan.positives {
		if ctx.Err() != nil {
			return
		}
		msg.SetQuestion(check.domain, check.qtype)
		reply, reason, retries := client.validationLookup(ctx, &msg, server.address, dns.RcodeSuccess)
		r.ValidationChecks++
		r.ValidationRetries += retries
		if reason == "" {
			var addresses []string
			addresses, reason = answerAddresses(reply, check.domain, check.qtype)
			if reason == "" && !slices.Equal(addresses, check.addresses) {
				reason = "positive_mismatch"
			}
		}
		if reason != "" {
			reject(reason, check.domain, check.qtype)
			return
		}
	}
	for _, name := range plan.negatives {
		for _, qtype := range plan.qtypes {
			if ctx.Err() != nil {
				return
			}
			msg.SetQuestion(name, qtype)
			_, reason, retries := client.validationLookup(ctx, &msg, server.address, dns.RcodeNameError)
			r.ValidationChecks++
			r.ValidationRetries += retries
			if reason != "" {
				reject("negative_"+reason, name, qtype)
				return
			}
		}
	}
}
