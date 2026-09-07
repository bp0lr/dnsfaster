package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxSourceBytes = 32 << 20

func isRemoteSource(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

func openSource(ctx context.Context, source string) (io.ReadCloser, error) {
	if !isRemoteSource(source) {
		return os.Open(source)
	}
	location, err := url.Parse(source)
	if err != nil || location.Host == "" || location.User != nil {
		return nil, errors.New("source must be an HTTP(S) URL without embedded credentials")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("too many source redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("invalid source redirect scheme")
		}
		if req.URL.User != nil || (via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https") {
			return errors.New("unsafe source redirect")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download source: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxSourceBytes {
		return nil, errors.New("source exceeds 32 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if len(data) > maxSourceBytes {
		return nil, errors.New("source exceeds 32 MiB")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type exclusionSet struct {
	hosts, endpoints map[string]bool
	networks         []netip.Prefix
}

func parseExclusions(values []string) (exclusionSet, error) {
	set := exclusionSet{hosts: make(map[string]bool), endpoints: make(map[string]bool)}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(value, "/") {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return set, fmt.Errorf("invalid exclusion %q: %w", value, err)
			}
			if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
				prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
			}
			set.networks = append(set.networks, prefix.Masked())
			continue
		}
		ep, err := parseEndpoint(value)
		if err != nil {
			return set, fmt.Errorf("invalid exclusion %q: %w", value, err)
		}
		_, portErr := netip.ParseAddr(value)
		hostOnly := portErr == nil || !strings.Contains(value, ":") || (strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"))
		if hostOnly {
			host, _, _ := net.SplitHostPort(ep.address)
			set.hosts[host] = true
		} else {
			set.endpoints[ep.address] = true
		}
	}
	return set, nil
}

func (set exclusionSet) contains(ep endpoint) bool {
	if set.endpoints[ep.address] {
		return true
	}
	host, _, _ := net.SplitHostPort(ep.address)
	if set.hosts[host] {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err == nil {
		for _, network := range set.networks {
			if network.Contains(ip.WithZone("")) {
				return true
			}
		}
	}
	return false
}

func loadResolvers(ctx context.Context, c config, stdin io.Reader) ([]endpoint, int, error) {
	var servers []endpoint
	if c.input != "" {
		input := stdin
		if c.input != "-" {
			source, err := openSource(ctx, c.input)
			if err != nil {
				return nil, 0, err
			}
			defer source.Close()
			input = source
		}
		var err error
		if closer, ok := input.(io.Closer); ok {
			stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
			defer stop()
		}
		servers, err = readResolvers(input)
		if err != nil {
			return nil, 0, err
		}
	}
	for _, value := range c.resolverInputs {
		ep, err := parseEndpoint(strings.TrimSpace(value))
		if err != nil {
			return nil, 0, fmt.Errorf("--resolver: %w", err)
		}
		servers = append(servers, ep)
	}
	exclusions := append([]string(nil), c.exclusions...)
	if c.excludeFile != "" {
		source, err := openSource(ctx, c.excludeFile)
		if err != nil {
			return nil, 0, fmt.Errorf("exclusions: %w", err)
		}
		defer source.Close()
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			value := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(scanner.Text(), "\ufeff"), "#", 2)[0])
			if value != "" {
				exclusions = append(exclusions, value)
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, 0, fmt.Errorf("read exclusions: %w", err)
		}
	}
	excluded, err := parseExclusions(exclusions)
	if err != nil {
		return nil, 0, err
	}
	seen := make(map[string]bool, len(servers))
	selected := make([]endpoint, 0, len(servers))
	count := 0
	for _, ep := range servers {
		if seen[ep.address] {
			continue
		}
		seen[ep.address] = true
		if excluded.contains(ep) {
			count++
			continue
		}
		selected = append(selected, ep)
	}
	if len(selected) == 0 {
		return nil, count, errors.New("no resolvers remain after exclusions")
	}
	return selected, count, nil
}
