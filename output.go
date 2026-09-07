package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
)

type outputFile struct {
	file      *os.File
	target    string
	committed bool
}

func prepareOutput(target, input string) (*outputFile, error) {
	targetPath, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	if input != "-" {
		inputPath, err := filepath.Abs(input)
		if err != nil {
			return nil, err
		}
		if targetPath == inputPath {
			return nil, errors.New("input and output must be different files")
		}
		a, ea := os.Stat(inputPath)
		b, eb := os.Stat(targetPath)
		if ea == nil && eb == nil && os.SameFile(a, b) {
			return nil, errors.New("input and output refer to the same file")
		}
	}
	mode := os.FileMode(0600)
	if info, err := os.Lstat(targetPath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("output must be a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(targetPath), ".dnsfaster-*")
	if err != nil {
		return nil, fmt.Errorf("prepare output: %w", err)
	}
	output := &outputFile{file: f, target: targetPath}
	if err := f.Chmod(mode); err != nil {
		output.abort()
		return nil, err
	}
	return output, nil
}

func (o *outputFile) abort() {
	if !o.committed {
		_ = o.file.Close()
		_ = os.Remove(o.file.Name())
	}
}

func (o *outputFile) commit() error {
	if err := o.file.Sync(); err != nil {
		return err
	}
	if err := o.file.Close(); err != nil {
		return err
	}
	// Rename a complete file without deleting the previous destination first.
	if err := os.Rename(o.file.Name(), o.target); err != nil {
		return fmt.Errorf("replace output: %w", err)
	}
	o.committed = true
	return nil
}

func writeResults(dest io.Writer, results []resultStats, format string, includeFiltered bool) error {
	selected := make([]resultStats, 0, len(results))
	for _, r := range results {
		if includeFiltered || !r.Filtered {
			selected = append(selected, r)
		}
	}
	if format == "json" {
		encoder := json.NewEncoder(dest)
		encoder.SetIndent("", "  ")
		return encoder.Encode(selected)
	}
	if format == "csv" || format == "legacy-csv" {
		w := csv.NewWriter(dest)
		if format == "csv" {
			if err := w.Write([]string{"resolver", "average_ms", "success_percent", "successes", "failures", "p50_ms", "p95_ms", "precheck_failures", "filtered", "reasons", "errors", "validation_checks", "validation_failures", "validation_retries"}); err != nil {
				return err
			}
		}
		for _, r := range selected {
			row := []string{r.Resolver, decimal(r.AverageMS), decimal(r.SuccessRate), strconv.Itoa(r.Successes), strconv.Itoa(r.Failures)}
			if format == "csv" {
				details, err := json.Marshal(r.Errors)
				if err != nil {
					return err
				}
				row = append(row, decimal(r.P50MS), decimal(r.P95MS), strconv.Itoa(r.PrecheckFailures), strconv.FormatBool(r.Filtered), strings.Join(r.Reasons, ";"), string(details))
				row = append(row, strconv.Itoa(r.ValidationChecks), strconv.Itoa(r.ValidationFailures), strconv.Itoa(r.ValidationRetries))
			}
			if err := w.Write(row); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	}
	w := bufio.NewWriter(dest)
	for _, r := range selected {
		if _, err := fmt.Fprintln(w, r.Resolver); err != nil {
			return err
		}
	}
	return w.Flush()
}

func selectExports(results []resultStats, top int, includeFiltered bool) []resultStats {
	selected := make([]resultStats, 0, len(results))
	passing := 0
	for _, r := range results {
		if r.Filtered {
			if includeFiltered {
				selected = append(selected, r)
			}
			continue
		}
		if top == 0 || passing < top {
			selected = append(selected, r)
			passing++
		}
	}
	return selected
}

func decimal(n float64) string { return strconv.FormatFloat(n, 'f', 3, 64) }

func writeReport(dest io.Writer, results []resultStats) error {
	w := tabwriter.NewWriter(dest, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "STATUS\tRESOLVER\tAVG MS\tP95 MS\tSUCCESS %\tOK\tFAIL\tPRECHECK FAIL\tREASONS"); err != nil {
		return err
	}
	for _, r := range results {
		status := "OK"
		if r.Filtered {
			status = "FILTER"
		}
		avg, p95 := decimal(r.AverageMS), decimal(r.P95MS)
		if r.Successes == 0 {
			avg, p95 = "n/a", "n/a"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n", status, r.Resolver, avg, p95, decimal(r.SuccessRate), r.Successes, r.Failures, r.PrecheckFailures, strings.Join(r.Reasons, ",")); err != nil {
			return err
		}
	}
	return w.Flush()
}
