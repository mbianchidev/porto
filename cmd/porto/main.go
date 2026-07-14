package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/daemon"
	"github.com/mbianchidev/porto/internal/discovery"
	"github.com/mbianchidev/porto/internal/gitutil"
	"github.com/mbianchidev/porto/internal/killswitch"
	"github.com/mbianchidev/porto/internal/sqnsl"
	"github.com/mbianchidev/porto/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "porto:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "scan":
		return scan(db, args[1:])
	case "list", "status":
		return list(db)
	case "start", "stop", "restart", "kill":
		return projectAction(args[0], args[1:])
	case "logs":
		return logs(db, args[1:])
	case "branch":
		return branch(args[1:])
	case "port":
		return pinPort(args[1:])
	case "kill-switch", "killswitch":
		return killSwitchCmd(db, args[1:])
	case "daemon":
		return daemonCmd(db, args[1:])
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func openStore() (*store.Store, error) {
	path, err := config.DBPath()
	if err != nil {
		return nil, err
	}
	return store.Open(path)
}

func scan(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	depth := fs.Int("depth", config.DefaultScanDepth, "scan depth")
	ignore := fs.String("ignore", ".git,vendor,dist,target", "comma-separated ignore directories")
	if err := fs.Parse(args); err != nil {
		return err
	}
	roots := fs.Args()
	if len(roots) == 0 {
		roots = []string{"."}
	}
	if daemonUp() {
		return api("POST", "/api/scan", map[string]any{"roots": roots, "depth": *depth, "ignore": strings.Split(*ignore, ",")}, os.Stdout)
	}
	projects, err := discovery.Scan(context.Background(), roots, discovery.Options{Depth: *depth, Ignore: strings.Split(*ignore, ",")})
	if err != nil {
		return err
	}
	for _, p := range projects {
		_, _ = st.UpsertProject(context.Background(), p)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		return err
	}
	if settings.SQLNotSoLiteEnabled {
		allProjects, err := st.ListProjects(context.Background())
		if err != nil {
			return err
		}
		result, err := sqnsl.NewManager(nil).Sync(context.Background(), allProjects)
		if err != nil {
			fmt.Fprintf(os.Stderr, "porto: sql-not-so-lite integration: %v\n", err)
		} else if result.Output != "" {
			fmt.Println(result.Output)
		}
	}
	fmt.Printf("discovered %d project(s)\n", len(projects))
	return nil
}

func list(st *store.Store) error {
	if daemonUp() {
		return api("GET", "/api/projects", nil, os.Stdout)
	}
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("%-4s %-22s %-9s %-7s %-8s %-16s %s\n", "ID", "NAME", "STATUS", "PORT", "BRANCH", "STRATEGY", "PATH")
	for _, p := range projects {
		fmt.Printf("%-4d %-22s %-9s %-7d %-8s %-16s %s\n", p.ID, p.Name, p.Status, p.Port, gitutil.Branch(p.Path), p.Strategy, p.Path)
	}
	return nil
}

func projectAction(action string, args []string) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	noPull := fs.Bool("no-pull", false, "skip git pull")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: porto %s <project>", action)
	}
	path := fmt.Sprintf("/api/projects/%s/%s", fs.Arg(0), action)
	if *noPull {
		path += "?noPull=1"
	}
	if !daemonUp() {
		return fmt.Errorf("daemon is not running; start it with 'porto daemon start'")
	}
	return api("POST", path, nil, os.Stdout)
}

func logs(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	limit := fs.Int("n", 200, "lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: porto logs <project>")
	}
	if daemonUp() {
		return api("GET", fmt.Sprintf("/api/projects/%s/logs?limit=%d", fs.Arg(0), *limit), nil, os.Stdout)
	}
	p, err := st.GetProject(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	lines, err := st.Logs(context.Background(), p.ID, *limit)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Printf("%s %-6s %s\n", l.CreatedAt.Format(time.Kitchen), l.Stream, l.Line)
	}
	return nil
}

func branch(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: porto branch <project> <branch>")
	}
	if !daemonUp() {
		return fmt.Errorf("daemon is not running")
	}
	return api("POST", fmt.Sprintf("/api/projects/%s/branch", args[0]), map[string]string{"branch": args[1]}, os.Stdout)
}

func pinPort(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: porto port <project> <port>")
	}
	port, err := strconv.Atoi(args[1])
	if err != nil {
		return err
	}
	if !daemonUp() {
		return fmt.Errorf("daemon is not running")
	}
	return api("POST", fmt.Sprintf("/api/projects/%s/port", args[0]), map[string]int{"port": port}, os.Stdout)
}

func killSwitchCmd(st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: porto kill-switch status|install|sync|cleanup")
	}
	action := args[0]
	if daemonUp() {
		method := "POST"
		path := "/api/integrations/kill-switch"
		if action == "status" {
			method = "GET"
		} else {
			path += "/" + action
		}
		switch action {
		case "status", "install", "sync", "cleanup":
			return api(method, path, nil, os.Stdout)
		default:
			return fmt.Errorf("unsupported KillSwitch command %q", action)
		}
	}

	manager := killswitch.NewManager(nil, nil)
	ctx := context.Background()
	switch action {
	case "status":
		status, err := manager.Probe(ctx)
		if encodeErr := writeOutput(status); encodeErr != nil {
			return encodeErr
		}
		return err
	case "install":
		status, err := manager.Install(ctx)
		if encodeErr := writeOutput(status); encodeErr != nil {
			return encodeErr
		}
		return err
	case "sync":
		return errors.New("daemon is not running; start it with 'porto daemon start' before syncing KillSwitch ports")
	case "cleanup":
		settings, err := st.Settings(ctx)
		if err != nil {
			return err
		}
		if !settings.KillSwitchEnabled {
			return errors.New("KillSwitch integration is disabled")
		}
		result, err := manager.Cleanup(ctx)
		if err != nil {
			return err
		}
		return writeOutput(result)
	default:
		return fmt.Errorf("unsupported KillSwitch command %q", action)
	}
}

func writeOutput(value any) error {
	return json.NewEncoder(os.Stdout).Encode(value)
}

func daemonCmd(st *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: porto daemon start|status")
	}
	switch args[0] {
	case "start":
		return daemon.New(st, dashboardFS()).Run(context.Background())
	case "status":
		if daemonUp() {
			fmt.Println("running")
			return nil
		}
		fmt.Println("stopped")
		return nil
	default:
		return fmt.Errorf("unsupported daemon command %q", args[0])
	}
}

func dashboardFS() fs.FS {
	candidates := []string{}
	if dir := os.Getenv("PORTO_UI_DIR"); dir != "" {
		candidates = append(candidates, dir)
	}
	candidates = append(candidates, "ui/dist")
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(base, "ui", "dist"), filepath.Join(base, "dist"))
	}
	for _, dir := range candidates {
		if info, err := os.Stat(filepath.Join(dir, "index.html")); err == nil && !info.IsDir() {
			return os.DirFS(dir)
		}
	}
	return nil
}

func daemonUp() bool {
	c := http.Client{Timeout: 250 * time.Millisecond}
	resp, err := c.Get("http://" + config.DaemonAddr + "/api/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func api(method, path string, body any, out io.Writer) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+config.DaemonAddr+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func usage() {
	fmt.Println(`Porto — local project orchestrator

Commands:
  porto scan [roots...] --depth 3 [--ignore .git,vendor,dist,target]
  porto list
  porto daemon start|status
  porto start|stop|restart|kill <project> [--no-pull]
  porto logs <project> [-n 200]
  porto branch <project> <branch>
  porto port <project> <port>
  porto kill-switch status|install|sync|cleanup`)
}
