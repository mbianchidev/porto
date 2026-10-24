package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localhttps"
	"github.com/mbianchidev/porto/internal/networking"
)

func networkingCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	if len(args) != 1 {
		return errors.New("usage: porto networking status|configure|ca-import|apply|inspect")
	}
	manager, err := networking.Default()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ca-import":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, networking.MaxRegistryCABytes+1))
		if err != nil {
			return err
		}
		certificate, err := manager.ImportCertificate(data)
		if err != nil {
			return err
		}
		return writeOutput(certificate)
	case "status":
		if daemonUp() {
			return runtimeGET("/api/networking")
		}
		proxy, err := manager.Snapshot()
		if err != nil {
			return err
		}
		certificate, _, err := config.CertificatePaths()
		if err != nil {
			return err
		}
		return writeOutput(map[string]any{"proxy": proxy, "https": localhttps.Snapshot(certificate), "message": "Daemon is stopped; engine networking is not probed"})
	case "configure":
		var configuration networking.Config
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1024*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&configuration); err != nil {
			return err
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("network configuration must contain exactly one JSON object")
		}
		if err := manager.Save(configuration); err != nil {
			return err
		}
		status, err := manager.Snapshot()
		if err != nil {
			return err
		}
		return writeOutput(status)
	case "apply":
		return runtimePOST("/api/networking/apply", nil)
	case "inspect":
		if daemonUp() {
			return runtimeGET("/api/networking/connectivity")
		}
		return writeOutput(networking.InspectHost(context.Background()))
	default:
		return errors.New("usage: porto networking status|configure|ca-import|apply|inspect")
	}
}
