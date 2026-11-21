package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type settings struct {
	Listen  string `toml:"listen"`
	Workers int    `toml:"workers"`
	Wait    string `toml:"wait"`
	Adapter string `toml:"adapter"`
	Events  string `toml:"events"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	args := os.Args[1:]
	switch {
	case filepath.Base(os.Args[0]) == "ocean-worker" || (len(args) > 0 && args[0] == "--ocean-worker"):
		if len(args) > 0 && args[0] == "--ocean-worker" {
			args = args[1:]
		}
		err = workerMain(ctx, args)
	case len(args) == 1 && strings.HasPrefix(args[0], "--ocean-internal-"):
		err = bridge(args[0])
	default:
		err = oceanMain(ctx, args)
	}
	if err == nil {
		return
	}
	var result *exec.ExitError
	if errors.As(err, &result) {
		code := result.ExitCode()
		if code < 0 {
			if status, ok := result.Sys().(syscall.WaitStatus); ok {
				code = 128 + int(status.Signal())
			}
		}
		os.Exit(code)
	}
	fmt.Fprintf(os.Stderr, "ocean: %v\n", err)
	os.Exit(1)
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func debugf(format string, args ...any) {
	if os.Getenv("OCEAN_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "ocean: "+format+"\n", args...)
	}
}

func commandText(ctx context.Context, path string, args ...string) (string, error) {
	data, err := exec.CommandContext(ctx, path, args...).Output()
	return strings.TrimSpace(string(data)), err
}

func bridge(operation string) error {
	conn, err := net.Dial("unix", os.Getenv("OCEAN_SOCKET"))
	if err != nil {
		return err
	}
	defer conn.Close()
	w := newWire(conn)
	input, err := io.ReadAll(io.LimitReader(os.Stdin, maxFrame))
	if err != nil {
		return err
	}
	req := message{}
	switch operation {
	case "--ocean-internal-graph":
		req.Type = "graph"
		var resolved struct {
			Root, CWD string
			Graph     map[string][]string
		}
		if err := json.Unmarshal(input, &resolved); err != nil || resolved.Graph == nil {
			// A previously built Lake adapter sends just the graph.
			if err := json.Unmarshal(input, &req.Graph); err != nil {
				return err
			}
		} else {
			req.Root, req.CWD, req.Graph = resolved.Root, resolved.CWD, resolved.Graph
		}
	case "--ocean-internal-compile":
		req.Type = "compile"
		req.Job = &compileJob{Setup: input}
	default:
		return errors.New("unknown bridge command")
	}
	if err := w.send(req); err != nil {
		return err
	}
	msg, err := w.receive()
	if err != nil {
		return err
	}
	if req.Type == "graph" {
		if msg.Type != "ack" {
			return errors.New("graph notification rejected")
		}
		return nil
	}
	if msg.Type != "complete" || msg.Result == nil || msg.Result.Fallback {
		return errors.New("delegating to local compiler")
	}
	return json.NewEncoder(os.Stdout).Encode(msg.Result)
}

func oceanMain(ctx context.Context, args []string) error {
	start := time.Now()
	var config string
	policy := "eager"
	for len(args) > 0 && strings.HasPrefix(args[0], "--ocean-") {
		key, val, hasValue := strings.Cut(args[0], "=")
		args = args[1:]
		if key != "--ocean-config" && key != "--ocean-policy" {
			return fmt.Errorf("unknown Ocean option %s", key)
		}
		if !hasValue {
			if len(args) == 0 {
				return fmt.Errorf("%s requires a value", key)
			}
			val, args = args[0], args[1:]
		}
		if key == "--ocean-config" {
			config = val
		} else {
			policy = val
		}
	}
	if policy != "eager" && policy != "single" && policy != "delayed" {
		return fmt.Errorf("unknown scheduling policy %q", policy)
	}
	lake, err := exec.LookPath("lake")
	if err != nil {
		return err
	}
	stock := func() error { return syscall.Exec(lake, append([]string{"lake"}, args...), os.Environ()) }
	if config == "" || !buildInvocation(args) {
		return stock()
	}
	cfg := settings{Listen: "127.0.0.1:7431", Workers: 1, Wait: "3s"}
	data, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	if err := toml.NewDecoder(strings.NewReader(string(data))).DisallowUnknownFields().Decode(&cfg); err != nil {
		return err
	}
	wait, err := time.ParseDuration(cfg.Wait)
	if err != nil || wait <= 0 || cfg.Workers < 1 {
		return errors.New("worker count and registration wait must be positive")
	}
	leanArgs, lakeArgs := []string{}, args
	if len(args) > 0 && strings.HasPrefix(args[0], "+") {
		leanArgs, lakeArgs = args[:1], args[1:]
	}
	root, err := commandText(ctx, "lean", append(leanArgs, "--print-prefix")...)
	if err != nil {
		return stock()
	}
	hash, err := commandText(ctx, filepath.Join(root, "bin", "lean"), "--githash")
	if err != nil || !strings.HasPrefix(hash, "eb6cafb") || runtime.GOOS != "linux" {
		return stock()
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if cfg.Adapter == "" {
		cfg.Adapter = filepath.Join(filepath.Dir(self), "ocean-lake")
	} else if !filepath.IsAbs(cfg.Adapter) {
		cfg.Adapter = filepath.Join(filepath.Dir(config), cfg.Adapter)
	}
	adapter, err := filepath.Abs(cfg.Adapter)
	if err != nil {
		return err
	}
	if _, err := os.Stat(adapter); err != nil {
		debugf("Lake adapter unavailable: %v", err)
		return stock()
	}
	tcp, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer tcp.Close()
	tmp, err := os.MkdirTemp("", "ocean-master-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	socket := filepath.Join(tmp, "control.sock")
	unix, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer unix.Close()
	var events *os.File
	if cfg.Events != "" {
		events, err = os.Create(cfg.Events)
		if err != nil {
			return err
		}
		defer events.Close()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newMaster(runCtx, root, hash, policy, events)
	m.tempDir = tmp
	go m.listen(tcp, m.register)
	go m.listen(unix, m.local)
	go m.clock()
	if !m.waitWorkers(cfg.Workers, wait) {
		debugf("no compatible workers registered")
		cancel()
		tcp.Close()
		unix.Close()
		os.RemoveAll(tmp)
		if events != nil {
			events.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return stock()
	}
	cmd := exec.CommandContext(ctx, adapter, lakeArgs...)
	cmd.Env = append(os.Environ(), "LEAN_SYSROOT="+root, "OCEAN_BRIDGE="+self, "OCEAN_SOCKET="+socket)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	wall := time.Since(start)
	m.mu.Lock()
	m.draining = true
	for _, worker := range m.workers {
		if worker.busy && worker.cache.seeding {
			worker.w.Close()
		}
	}
	m.mu.Unlock()
	if err != nil {
		cancel()
	}
	m.running.Wait()
	m.event("build-end", "", "", 0, "wallNanos", wall.Nanoseconds(), "success", err == nil)
	return err
}

func buildInvocation(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if i == 0 && strings.HasPrefix(arg, "+") {
			continue
		}
		switch arg {
		case "--help", "-h", "--version", "--":
			return false
		case "-d", "--dir", "-f", "--file", "-K", "--packages", "--log-level", "--fail-level":
			i++
		default:
			if !strings.HasPrefix(arg, "-") {
				return arg == "build"
			}
		}
	}
	return false
}
