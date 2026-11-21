package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type event struct {
	Event   string
	Module  string
	Host    string
	Attempt uint64
	Time    time.Time
}

func runCommand(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func loadEvents(t *testing.T, path string) []event {
	t.Helper()
	log, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var events []event
	scan := bufio.NewScanner(log)
	for scan.Scan() {
		var item event
		if err := json.Unmarshal(scan.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		events = append(events, item)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestCompose(t *testing.T) {
	policy := os.Getenv("FLAKE_COMPOSE")
	if policy == "" {
		t.Skip("run with scripts/test-compose")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "Lean project")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lakefile.toml", "lean-toolchain", "Base.lean", "Left.lean", "Right.lean", "Main.lean"} {
		data, err := os.ReadFile(filepath.Join("tests", "project", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "Base.lean" || name == "Left.lean" || name == "Right.lean" {
			data = append(data, []byte("\n#eval IO.sleep 350\n")...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	workers := 2
	if policy == "eager" {
		workers = 3
	}
	log := filepath.Join(dir, "events.jsonl")
	config := fmt.Sprintf("listen = \"0.0.0.0:7431\"\nworkers = %d\nwait = \"5s\"\nevents = %q\n", workers, log)
	if err := os.WriteFile(filepath.Join(dir, "workers.toml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	runCommand(t, dir, "lake", "env", "true")
	stock := runCommand(t, dir, "lake", "env", "printf", "%s", "argument with spaces")
	wrapped := runCommand(t, dir, "flake", "--flake-policy="+policy, "env", "printf", "%s", "argument with spaces")
	if !bytes.Equal(stock, wrapped) {
		t.Fatalf("pass-through output: %q, want %q", wrapped, stock)
	}
	runCommand(t, parent, "flake", "--flake-config="+filepath.Join(dir, "workers.toml"), "--flake-policy="+policy,
		"--dir="+filepath.Base(dir), "--no-cache", "build", "Main", "+Base")
	events := loadEvents(t, log)
	dispatch := map[string][]event{}
	published := map[string][]event{}
	seeded := map[string]bool{}
	for _, e := range events {
		switch e.Event {
		case "seed-end":
			seeded[e.Host] = true
		case "dispatch":
			if !seeded[e.Host] {
				t.Fatalf("compiled before receiving snapshot: %+v", e)
			}
			dispatch[e.Module] = append(dispatch[e.Module], e)
		case "publish":
			published[e.Module] = append(published[e.Module], e)
		}
	}
	for _, module := range []string{"Base", "Left", "Right", "Main"} {
		if len(dispatch[module]) == 0 || len(published[module]) != 1 {
			t.Fatalf("%s dispatches=%v publications=%v", module, dispatch[module], published[module])
		}
		if _, err := os.Stat(filepath.Join(dir, ".lake", "build", "lib", "lean", module+".olean")); err != nil {
			t.Fatal(err)
		}
	}
	left, right := dispatch["Left"][0], dispatch["Right"][0]
	base, main := published["Base"][0], dispatch["Main"][0]
	if base.Time.After(left.Time) || base.Time.After(right.Time) ||
		main.Time.Before(published["Left"][0].Time) || main.Time.Before(published["Right"][0].Time) {
		t.Fatalf("dependency order violated: %v", events)
	}
	if policy == "single" && (left.Host == right.Host || left.Time.After(published["Right"][0].Time) ||
		right.Time.After(published["Left"][0].Time)) {
		t.Fatalf("branches failed to overlap: %v", events)
	}
	if policy == "eager" {
		hosts := map[string]bool{}
		for _, attempt := range dispatch["Base"] {
			if attempt.Time.Before(base.Time) && attempt.Time.Sub(dispatch["Base"][0].Time) < time.Second {
				hosts[attempt.Host] = true
			}
		}
		if len(hosts) != 3 {
			t.Fatalf("wanted three eager replicas, got %v", dispatch["Base"])
		}
	}
	runCommand(t, dir, "lake", "--no-build", "--no-cache", "build", "Main")
	runCommand(t, parent, "flake", "--flake-config="+filepath.Join(dir, "workers.toml"), "--flake-policy="+policy,
		"--dir="+filepath.Base(dir), "--no-cache", "build", "Main", "+Base")
	graphReceived := false
	for _, e := range loadEvents(t, log) {
		if e.Event == "graph" {
			graphReceived = true
		}
		if e.Event == "snapshot" || e.Event == "seed-end" || e.Event == "dispatch" {
			t.Fatalf("already built targets triggered work: %+v", e)
		}
	}
	if !graphReceived {
		t.Fatal("up-to-date build did not exercise the Lake adapter")
	}
	if err := os.WriteFile(filepath.Join(dir, "Consumer.lean"), []byte("import Main\nexample : left + right = 5 := rfl\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runCommand(t, dir, "lake", "env", "lean", "Consumer.lean")
	if err := os.WriteFile(filepath.Join(dir, "Base.lean"), []byte("def base : MissingType := 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bad := exec.CommandContext(t.Context(), "flake", "--flake-config=workers.toml", "--flake-policy=single", "build", "Main")
	bad.Dir = dir
	out, err := bad.CombinedOutput()
	if err == nil || !bytes.Contains(out, []byte("MissingType")) {
		t.Fatalf("missing Lean failure or diagnostic: %v\n%s", err, out)
	}
}

func TestFrame(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	data := bytes.Repeat([]byte{0, 1, 10, 255}, chunkSize/3)
	if err := os.WriteFile(source, data, 0644); err != nil {
		t.Fatal(err)
	}
	entry, err := describe(source, "source")
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- streamFile(newWire(a), 9, source, entry) }()
	dest := filepath.Join(t.TempDir(), "out")
	output, err := readOutput(newWire(b), 9, entry, dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := publish([]stagedOutput{output}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("published contents differ")
	}
}

func TestDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := newMaster(ctx, "", "test", "single", nil)
	a, b := net.Pipe()
	defer b.Close()
	go m.register(newWire(a))
	worker := newWire(b)
	if err := worker.send(message{Type: "register", Version: protocolVersion, Host: "fake", Platform: platform(), Toolchain: "test", Slots: 1}); err != nil {
		t.Fatal(err)
	}
	if msg, err := worker.receive(); err != nil || msg.Type != "registered" {
		t.Fatalf("registration: %v, %v", msg, err)
	}
	if !m.waitWorkers(1, time.Second) {
		t.Fatal("worker did not register")
	}
	job := &pendingJob{
		spec: prepared{job: compileJob{Module: "Base"}},
		done: make(chan compileResult, 1), active: map[*slot]context.CancelFunc{}, hosts: map[string]bool{},
	}
	m.mu.Lock()
	m.pending = append(m.pending, job)
	m.schedule()
	m.mu.Unlock()
	if msg, err := worker.receive(); err != nil || msg.Type != "job" {
		t.Fatalf("assignment: %v, %v", msg, err)
	}
	b.Close()
	select {
	case result := <-job.done:
		if !result.Fallback {
			t.Fatalf("expected local fallback, got %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnected attempt did not unblock")
	}
}
