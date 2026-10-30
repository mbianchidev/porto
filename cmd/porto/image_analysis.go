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
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/imageanalysis"
)

type analysisExit struct {
	code    int
	message string
}

func (e analysisExit) Error() string { return e.message }
func (e analysisExit) ExitCode() int { return e.code }

func imageAnalysisCmd(args []string) error {
	err := runImageAnalysis(args)
	if err == nil {
		return nil
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return err
	}
	return analysisExit{2, err.Error()}
}

func runImageAnalysis(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto image-analysis scan|report|export|cancel|database")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "scan":
		flags := flag.NewFlagSet("image-analysis scan", flag.ContinueOnError)
		platform := flags.String("platform", "", "explicit locally available platform")
		threshold := flags.String("fail-on", "", "negligible, low, medium, high or critical")
		allowStale := flags.Bool("allow-stale", false, "permit stale matching; exit remains 2")
		maxAge := flags.Int("max-db-age-hours", 120, "maximum database age, 1-8760 hours")
		force := flags.Bool("force", false, "bypass cached analysis")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || (*threshold != "" && imageanalysis.SeverityRank(*threshold) < 0) {
			return errors.New("usage: porto image-analysis scan [--platform linux/arm64] [--fail-on high] [--allow-stale] [--force] IMAGE")
		}
		data, err := analysisRequest(ctx, http.MethodPost, "/api/docker/images/"+url.PathEscape(flags.Arg(0))+"/analysis", map[string]any{"platform": *platform, "allowStale": *allowStale, "maxAgeHours": *maxAge, "force": *force}, nil)
		if err != nil {
			return err
		}
		var report imageanalysis.Report
		if err := json.Unmarshal(data, &report); err != nil {
			return err
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for report.Status == "queued" || report.Status == "running" {
			select {
			case <-ctx.Done():
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, cancelErr := analysisRequest(cleanup, http.MethodPost, "/api/docker/analysis/"+url.PathEscape(report.ID)+"/cancel", nil, nil)
				cancel()
				return errors.Join(ctx.Err(), cancelErr)
			case <-ticker.C:
			}
			data, err = analysisRequest(ctx, http.MethodGet, "/api/docker/analysis/"+url.PathEscape(report.ID), nil, nil)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &report); err != nil {
				return err
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return err
		}
		if code := report.ExitCode(*threshold); code != 0 {
			return analysisExit{code, fmt.Sprintf("image analysis status %s; threshold %q (details in JSON output)", report.Status, *threshold)}
		}
		return nil
	case "report", "export", "cancel":
		flags := flag.NewFlagSet("image-analysis "+args[0], flag.ContinueOnError)
		format := flags.String("format", "report", "report, syft-json, spdx-json or cyclonedx-json")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("an analysis report ID is required")
		}
		path, method := "/api/docker/analysis/"+url.PathEscape(flags.Arg(0)), http.MethodGet
		if args[0] == "export" {
			path += "/export?format=" + url.QueryEscape(*format)
		} else if args[0] == "cancel" {
			path, method = path+"/cancel", http.MethodPost
		}
		data, err := analysisRequest(ctx, method, path, nil, nil)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	case "database":
		if len(args) < 2 {
			return errors.New("usage: porto image-analysis database status|update|import")
		}
		var data []byte
		var err error
		switch args[1] {
		case "status":
			if len(args) != 2 {
				return errors.New("database status takes no arguments")
			}
			data, err = analysisRequest(ctx, http.MethodGet, "/api/docker/analysis/database", nil, nil)
		case "update":
			flags := flag.NewFlagSet("database update", flag.ContinueOnError)
			consent := flags.Bool("confirm-network", false, "allow a bounded download from grype.anchore.io")
			maxMiB := flags.Int64("max-download-mib", 512, "download limit in MiB, at most 512")
			if err := flags.Parse(args[2:]); err != nil {
				return err
			}
			if !*consent || flags.NArg() != 0 || *maxMiB < 1 || *maxMiB > 512 {
				return errors.New("database update requires --confirm-network and a download limit of 1-512 MiB")
			}
			data, err = analysisRequest(ctx, http.MethodPost, "/api/docker/analysis/database/update", map[string]any{"confirmNetwork": true, "maxBytes": *maxMiB * 1024 * 1024}, nil)
		case "import":
			if len(args) != 3 {
				return errors.New("usage: porto image-analysis database import DATABASE.tar.zst")
			}
			file, err := os.Open(args[2])
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() > imageanalysis.MaxDatabaseBytes {
				return errors.New("offline database must be a regular archive of at most 512 MiB")
			}
			data, err = analysisRequest(ctx, http.MethodPut, "/api/docker/analysis/database/import", nil, file)
		default:
			return errors.New("unknown database action")
		}
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	default:
		return errors.New("unknown image-analysis action")
	}
}

func analysisRequest(ctx context.Context, method, path string, body any, stream io.Reader) ([]byte, error) {
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		stream = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+config.DaemonAddr+path, stream)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024+1))
	if err != nil || len(data) > 64*1024*1024 {
		return nil, errors.New("unable to read bounded analysis response")
	}
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("image analysis HTTP %d: %s", response.StatusCode, data)
	}
	return data, nil
}
