package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

type progressReporter struct {
	writer                   io.Writer
	started                  time.Time
	enabled, verbose         bool
	total, completed, passed int
}

func (p *progressReporter) phase(message string) error {
	if !p.enabled {
		return nil
	}
	_, err := fmt.Fprintf(p.writer, "Progress: %s; %d resolvers\n", message, p.total)
	return err
}

func (p *progressReporter) tick() error {
	_, err := fmt.Fprintf(p.writer, "Progress: %d/%d checked, %d passed, elapsed %s\n", p.completed, p.total, p.passed, time.Since(p.started).Round(time.Millisecond))
	return err
}

func (p *progressReporter) complete(r resultStats) error {
	p.completed++
	if !r.Filtered {
		p.passed++
	}
	if !p.verbose {
		return nil
	}
	status := "OK"
	if r.Filtered {
		status = "FILTER"
	}
	_, err := fmt.Fprintf(p.writer, "%s %s: %s\n", status, r.Resolver, strings.Join(r.Reasons, ","))
	return err
}

func writeSummary(dest io.Writer, results []resultStats, excluded, top int, started time.Time) error {
	passed := 0
	for _, r := range results {
		if !r.Filtered {
			passed++
		}
	}
	selected := passed
	if top > 0 {
		selected = min(selected, top)
	}
	_, err := fmt.Fprintf(dest, "Summary: %d checked, %d passed, %d rejected, %d excluded, %d selected; elapsed %s\n", len(results), passed, len(results)-passed, excluded, selected, time.Since(started).Round(time.Millisecond))
	return err
}
