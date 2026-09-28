package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/diagnostics"
	"github.com/mbianchidev/porto/internal/store"
)

type diagnosticStatusError struct {
	state diagnostics.State
	code  int
}

func (e diagnosticStatusError) Error() string {
	return fmt.Sprintf("diagnostics completed with %s status", e.state)
}

func (e diagnosticStatusError) ExitCode() int {
	return e.code
}

func diagnoseCmd(st *store.Store, storeErr error, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "bundle":
			return diagnoseBundle(st, storeErr, args[1:])
		case "repair":
			return diagnoseRepair(args[1:])
		}
	}
	flags := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "write the structured diagnostic report")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: porto diagnose [--json]")
	}
	report, _, err := loadDiagnosticReport(st, storeErr)
	if err != nil {
		return err
	}
	if *jsonOutput {
		if err := writeOutput(report); err != nil {
			return err
		}
	} else {
		writeDiagnosticReport(os.Stdout, report)
	}
	if code := report.ExitCode(); code != 0 {
		return diagnosticStatusError{state: report.Overall, code: code}
	}
	return nil
}

func diagnoseBundle(st *store.Store, storeErr error, args []string) error {
	flags := flag.NewFlagSet("diagnose bundle", flag.ContinueOnError)
	previewOnly := flags.Bool("preview", false, "show bundle contents without writing a file")
	outputPath := flags.String("output", "", "diagnostic ZIP path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: porto diagnose bundle [--preview] [--output path]")
	}
	report, collector, err := loadDiagnosticReport(st, storeErr)
	if err != nil {
		return err
	}
	sources := collector.BundleSources(context.Background())
	redactionContext := collector.RedactionContext()
	if *previewOnly {
		preview := diagnostics.PreviewBundle(report, sources, redactionContext)
		return writeDiagnosticBundlePreview(os.Stdout, preview)
	}
	path := strings.TrimSpace(*outputPath)
	if path == "" {
		path = "porto-diagnostics-" + report.GeneratedAt.Format("20060102T150405Z") + ".zip"
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve diagnostic bundle path: %w", err)
	}
	file, err := os.OpenFile(absolutePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create diagnostic bundle: %w", err)
	}
	preview, buildErr := diagnostics.BuildBundle(file, report, sources, redactionContext)
	closeErr := file.Close()
	if err := errors.Join(buildErr, closeErr); err != nil {
		_ = os.Remove(absolutePath)
		return err
	}
	fmt.Fprintln(os.Stdout, absolutePath)
	for _, warning := range preview.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	return nil
}

func diagnoseRepair(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto diagnose repair <action> [--target name] --confirm")
	}
	action := args[0]
	flags := flag.NewFlagSet("diagnose repair", flag.ContinueOnError)
	confirm := flags.Bool("confirm", false, "confirm the scoped repair")
	target := flags.String("target", "", "repair target, such as a managed cluster name")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !*confirm {
		return errors.New("usage: porto diagnose repair <action> [--target name] --confirm")
	}
	if !daemonUp() {
		return errors.New("daemon is not running; start it before requesting a repair")
	}
	return api(
		http.MethodPost,
		"/api/diagnostics/repair/"+action,
		map[string]any{"confirm": true, "target": *target},
		os.Stdout,
	)
}

func loadDiagnosticReport(st *store.Store, storeErr error) (diagnostics.Report, *diagnostics.Collector, error) {
	var settings diagnostics.SettingsReader
	if st != nil {
		settings = st
	}
	collector, err := diagnostics.NewLocalCollector(settings)
	if err != nil {
		return diagnostics.Report{}, nil, err
	}
	collector.SettingsError = storeErr
	if daemonUp() {
		report, err := fetchDiagnosticReport()
		if err == nil {
			collector.DaemonAvailable = true
			return report, collector, nil
		}
		collector.DaemonAvailable = true
		collector.DashboardReady = true
		collector.DaemonMessage = err.Error()
	}
	return collector.Collect(context.Background()), collector, nil
}

func fetchDiagnosticReport() (diagnostics.Report, error) {
	request, err := http.NewRequest(http.MethodGet, "http://"+config.DaemonAddr+"/api/diagnostics", nil)
	if err != nil {
		return diagnostics.Report{}, err
	}
	client := &http.Client{Timeout: 45 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return diagnostics.Report{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return diagnostics.Report{}, fmt.Errorf("diagnostics API returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var report diagnostics.Report
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return diagnostics.Report{}, fmt.Errorf("decode diagnostics API response: %w", err)
	}
	return report, nil
}

func writeDiagnosticReport(writer io.Writer, report diagnostics.Report) {
	fmt.Fprintf(writer, "Porto diagnostics: %s\n", strings.ToUpper(string(report.Overall)))
	fmt.Fprintf(
		writer,
		"healthy=%d degraded=%d unavailable=%d unsafe=%d\n\n",
		report.Summary.Healthy,
		report.Summary.Degraded,
		report.Summary.Unavailable,
		report.Summary.Unsafe,
	)
	for _, check := range report.Checks {
		fmt.Fprintf(
			writer,
			"%-11s %-18s %s: %s\n",
			strings.ToUpper(string(check.State)),
			check.Category,
			check.Name,
			check.Summary,
		)
		if check.Detail != "" {
			fmt.Fprintf(writer, "            %s\n", check.Detail)
		}
		if check.Repair != nil {
			fmt.Fprintf(
				writer,
				"            repair: porto diagnose repair %s%s --confirm\n",
				check.Repair.ID,
				diagnosticRepairTarget(check.Repair.Target),
			)
		}
	}
}

func diagnosticRepairTarget(target string) string {
	if strings.TrimSpace(target) == "" {
		return ""
	}
	return " --target " + target
}

func writeDiagnosticBundlePreview(writer io.Writer, preview diagnostics.BundlePreview) error {
	var output bytes.Buffer
	for _, entry := range preview.Entries {
		fmt.Fprintf(
			&output,
			"%s\t%d bytes\t%d redaction(s)\t%s\n",
			entry.Name,
			entry.Size,
			entry.Redactions,
			entry.Description,
		)
	}
	for _, warning := range preview.Warnings {
		fmt.Fprintf(&output, "warning\t%s\n", warning)
	}
	_, err := io.Copy(writer, &output)
	return err
}
