package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/mbianchidev/porto/internal/etcqlite"
)

func etcqliteCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto etcqlite status|enable|disable|create|start|stop|backup|backups|delete|restore|logs")
	}
	const base = "/api/integrations/etcqlite"
	switch args[0] {
	case "status", "backups":
		if len(args) != 1 {
			return fmt.Errorf("usage: porto etcqlite %s", args[0])
		}
		path := base
		if args[0] == "backups" {
			path += "/backups"
		}
		return runtimeGET(path)
	case "enable", "disable":
		if len(args) != 1 {
			return fmt.Errorf("usage: porto etcqlite %s", args[0])
		}
		return runtimePOST(base+"/"+args[0], nil)
	case "create":
		fs := flag.NewFlagSet("etcqlite create", flag.ContinueOnError)
		count := fs.Int("members", 1, "1 standalone member or 3 local quorum members")
		advertise := fs.String("advertise-host", "127.0.0.1", "explicit client DNS/IP advertisement; host.lima.internal for Lima guests")
		listen := fs.String("listen-host", "127.0.0.1", "local client bind address; non-loopback binding is explicit")
		if err := parseInterspersed(fs, args[1:], nil); err != nil {
			return err
		}
		if fs.NArg() != 1 || (*count != 1 && *count != 3) {
			return errors.New("usage: porto etcqlite create <name> [--members 1|3] [--advertise-host host.lima.internal --listen-host 0.0.0.0]")
		}
		return runtimePOST(base+"/instances", etcqlite.CreateRequest{
			Name: fs.Arg(0), Members: *count, AdvertiseHost: *advertise, ListenHost: *listen,
		})
	case "logs":
		fs := flag.NewFlagSet("etcqlite logs", flag.ContinueOnError)
		stream := fs.String("stream", "stdout", "stdout or stderr")
		tail := fs.Int("tail", 100, "maximum log lines, 1 to 1000")
		if err := parseInterspersed(fs, args[1:], nil); err != nil {
			return err
		}
		if fs.NArg() != 1 || (*stream != "stdout" && *stream != "stderr") || *tail < 1 || *tail > 1000 {
			return errors.New("usage: porto etcqlite logs <name> [--stream stdout|stderr] [--tail 100]")
		}
		query := url.Values{"stream": {*stream}, "tail": {strconv.Itoa(*tail)}}
		return runtimeGET(base + "/instances/" + url.PathEscape(fs.Arg(0)) + "/logs?" + query.Encode())
	case "start", "stop", "backup", "delete", "restore":
		fs := flag.NewFlagSet("etcqlite "+args[0], flag.ContinueOnError)
		id := fs.String("cluster-id", "", "pin the datastore identity; required for retained restoration")
		confirm := fs.Bool("confirm", false, "confirm verified backup and reversible removal, or explicit retained restoration")
		if err := parseInterspersed(fs, args[1:], map[string]bool{"confirm": true}); err != nil {
			return err
		}
		if fs.NArg() != 1 || ((args[0] == "delete" || args[0] == "restore") && !*confirm) || (args[0] == "restore" && *id == "") {
			return fmt.Errorf("usage: porto etcqlite %s <name> [--cluster-id ID] [--confirm]; delete/restore require --confirm and restore requires --cluster-id", args[0])
		}
		if *id == "" {
			var err error
			*id, err = etcqliteIdentity(fs.Arg(0))
			if err != nil {
				return err
			}
		}
		path := base + "/instances/" + url.PathEscape(fs.Arg(0))
		method := "POST"
		if args[0] == "delete" {
			method = "DELETE"
		} else {
			path += "/" + args[0]
		}
		if !daemonUp() {
			return errors.New("daemon is not running; start it with 'porto daemon start'")
		}
		return api(method, path, etcqlite.IdentityRequest{ClusterID: *id, Confirm: *confirm}, os.Stdout)
	default:
		return fmt.Errorf("unsupported Etcqlite command %q", args[0])
	}
}

func etcqliteIdentity(name string) (string, error) {
	if !daemonUp() {
		return "", errors.New("daemon is not running; start it with 'porto daemon start'")
	}
	var output bytes.Buffer
	if err := api("GET", "/api/integrations/etcqlite", nil, &output); err != nil {
		return "", err
	}
	var status etcqlite.Status
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		return "", fmt.Errorf("decode Etcqlite resource identity: %w", err)
	}
	if status.State == "disabled" {
		return "", etcqlite.ErrDisabled
	}
	for _, instance := range status.Instances {
		if instance.Name == name {
			return instance.ClusterID, nil
		}
	}
	return "", fmt.Errorf("Etcqlite instance %q not found; create it explicitly or use the retained identity for restore", name)
}
