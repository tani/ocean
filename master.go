package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type slot struct {
	w     *wire
	host  string
	busy  bool
	cache *hostCache
}

type hostCache struct {
	known      map[string]bool
	seeding    bool
	seeded     bool
	seedFailed bool
	inputRate  float64
}

type pendingJob struct {
	spec          prepared
	done          chan compileResult
	active        map[*slot]context.CancelFunc
	hosts         map[string]bool
	started       time.Time
	attempts      int
	limit         int
	compilerError *compileResult
}

type stagedOutput struct {
	temp string
	dest string
}

type master struct {
	ctx            context.Context
	toolchain      string
	fingerprint    string
	session        string
	policy         string
	events         *os.File
	mu             sync.Mutex
	running        sync.WaitGroup
	draining       bool
	workers        []*slot
	caches         map[string]*hostCache
	pending        []*pendingJob
	ranks          map[string]int
	next           uint64
	durations      time.Duration
	index          inputIndex
	tempDir        string
	sourceRoot     string
	sourceSubdir   string
	snapshot       *sourcePack
	snapshotFailed bool
}

func newMaster(ctx context.Context, root, fingerprint, policy string, events *os.File) *master {
	var session [32]byte
	_, _ = rand.Read(session[:])
	return &master{ctx: ctx, toolchain: root, fingerprint: fingerprint,
		session: hex.EncodeToString(session[:]), policy: policy, events: events,
		ranks: map[string]int{}, caches: map[string]*hostCache{},
		index: inputIndex{files: map[string]cachedFile{}}}
}

func (m *master) event(kind, module, host string, id uint64, fields ...any) {
	if m.events == nil {
		return
	}
	value := map[string]any{"event": kind, "time": time.Now().UTC(), "module": module, "host": host, "attempt": id}
	for i := 0; i < len(fields); i += 2 {
		value[fields[i].(string)] = fields[i+1]
	}
	_ = json.NewEncoder(m.events).Encode(value)
}

func (m *master) listen(listener net.Listener, handle func(*wire)) {
	defer listener.Close()
	stop := context.AfterFunc(m.ctx, func() { listener.Close() })
	defer stop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handle(newWire(conn))
	}
}

func (m *master) register(w *wire) {
	_ = w.SetReadDeadline(time.Now().Add(3 * time.Second))
	msg, err := w.receive()
	if err != nil || msg.Type != "register" || msg.Version != protocolVersion || msg.Host == "" ||
		msg.Platform != platform() || msg.Toolchain != m.fingerprint || msg.Slots < 1 {
		_ = w.send(message{Type: "reject", Error: "toolchain, platform or protocol mismatch"})
		w.Close()
		return
	}
	_ = w.SetReadDeadline(time.Time{})
	if err := w.send(message{Type: "registered", Hash: m.session}); err != nil {
		w.Close()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cacheKey := msg.Host + "/" + msg.Hash
	if m.caches[cacheKey] == nil {
		m.caches[cacheKey] = &hostCache{known: map[string]bool{}}
	}
	m.workers = append(m.workers, &slot{w: w, host: msg.Host, cache: m.caches[cacheKey]})
	m.event("register", "", msg.Host, 0, "slots", msg.Slots)
	m.schedule()
}

func (m *master) waitWorkers(minimum int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		m.mu.Lock()
		hosts := map[string]bool{}
		for _, worker := range m.workers {
			hosts[worker.host] = true
		}
		m.mu.Unlock()
		if len(hosts) >= minimum {
			return true
		}
		select {
		case <-deadline.C:
			return len(hosts) != 0
		case <-m.ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

func (m *master) local(w *wire) {
	defer w.Close()
	msg, err := w.receive()
	if err != nil {
		return
	}
	if msg.Type == "graph" {
		m.mu.Lock()
		m.ranks = graphRanks(msg.Graph)
		m.event("graph", "", "", 0, "modules", len(m.ranks))
		if len(msg.Graph) != 0 && msg.Root != "" {
			m.prime(msg.Root, msg.CWD)
		}
		m.mu.Unlock()
		_ = w.send(message{Type: "ack"})
		return
	}
	if msg.Type != "compile" || msg.Job == nil {
		return
	}
	var input invocation
	if err := json.Unmarshal(msg.Job.Setup, &input); err != nil {
		return
	}
	p, err := prepare(input, m.toolchain, &m.index)
	if err != nil {
		debugf("%s remains local: %v", input.Module, err)
		_ = w.send(message{Type: "complete", Result: &compileResult{Fallback: true}})
		return
	}
	job := &pendingJob{spec: p, done: make(chan compileResult, 1), active: map[*slot]context.CancelFunc{}, hosts: map[string]bool{}}
	m.mu.Lock()
	if !m.prime(p.root, input.CWD) {
		m.mu.Unlock()
		_ = w.send(message{Type: "complete", Result: &compileResult{Fallback: true}})
		return
	}
	m.pending = append(m.pending, job)
	m.schedule()
	m.mu.Unlock()
	select {
	case result := <-job.done:
		_ = w.send(message{Type: "complete", Result: &result})
	case <-m.ctx.Done():
	}
}

// The caller holds m.mu. The graph arrives before Lake builds local prerequisites.
func (m *master) prime(root, cwd string) bool {
	root = filepath.Clean(root)
	subdir := ""
	if _, ok := inside(root, cwd); !ok {
		rel, ok := inside(cwd, root)
		if !ok {
			return false
		}
		subdir = rel
	}
	if m.sourceRoot != "" {
		return m.sourceRoot == root && m.sourceSubdir == subdir && !m.snapshotFailed
	}
	if m.draining {
		return false
	}
	m.sourceRoot, m.sourceSubdir = root, subdir
	m.running.Go(func() {
		start := time.Now()
		files, paths, err := m.index.snapshot(root)
		var pack sourcePack
		if err == nil {
			pack, err = packSources(m.tempDir, files, paths)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			m.snapshotFailed = true
			debugf("source snapshot: %v", err)
		} else {
			m.snapshot = &pack
			m.event("snapshot", "", "", 0, "files", len(files), "bytes", pack.entry.Size,
				"wallNanos", time.Since(start).Nanoseconds())
		}
		m.schedule()
	})
	return true
}

func (m *master) seed(worker *slot, pack sourcePack, subdir string) {
	_ = worker.w.SetDeadline(time.Now().Add(45 * time.Minute))
	id := worker.w.request.Add(1)
	start := time.Now()
	sent, received := worker.w.sent.Load(), worker.w.received.Load()
	err := worker.w.send(message{Type: "seed", ID: id, CWD: subdir,
		Files: slices.Concat([]fileEntry{pack.entry}, pack.files)})
	if err == nil {
		err = streamFile(worker.w, id, pack.path, pack.entry)
	}
	if err == nil {
		msg, recvErr := worker.w.receive()
		if recvErr != nil {
			err = recvErr
		} else if msg.Type != "seeded" || msg.ID != id {
			err = errors.New("worker did not verify source snapshot")
		}
	}
	// Clear the seeding deadline before another attempt acquires this slot.
	_ = worker.w.SetDeadline(time.Time{})
	m.mu.Lock()
	defer m.mu.Unlock()
	worker.busy, worker.cache.seeding = false, false
	m.event("seed-end", "", worker.host, 0, "success", err == nil,
		"wallNanos", time.Since(start).Nanoseconds(), "bytesSent", worker.w.sent.Load()-sent,
		"bytesReceived", worker.w.received.Load()-received)
	if err != nil {
		worker.cache.seedFailed = true
		worker.w.Close()
		m.workers = slices.DeleteFunc(m.workers, func(w *slot) bool { return w == worker })
		debugf("source snapshot on %s: %v", worker.host, err)
	} else {
		worker.cache.seeded = true
		for _, file := range pack.files {
			worker.cache.known[file.Hash] = true
		}
	}
	m.schedule()
}

func graphRanks(graph map[string][]string) map[string]int {
	reverse := map[string][]string{}
	for module, deps := range graph {
		for _, dep := range deps {
			reverse[dep] = append(reverse[dep], module)
		}
	}
	ranks := map[string]int{}
	var visit func(string) int
	visit = func(module string) int {
		if n := ranks[module]; n != 0 {
			return n
		}
		ranks[module] = 1
		for _, consumer := range reverse[module] {
			ranks[module] = max(ranks[module], 1+visit(consumer))
		}
		return ranks[module]
	}
	for module := range graph {
		visit(module)
	}
	return ranks
}

func (m *master) clock() {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, worker := range m.workers {
			worker.w.Close()
		}
	}()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-tick.C:
			if m.policy == "delayed" {
				m.mu.Lock()
				m.schedule()
				m.mu.Unlock()
			}
		}
	}
}

func (m *master) canStart(job *pendingJob, worker *slot) bool {
	if job.hosts[worker.host] || (job.limit != 0 && job.attempts >= job.limit) {
		return false
	}
	if len(job.active) == 0 {
		return true
	}
	switch m.policy {
	case "eager":
		return true
	case "delayed":
		return !job.started.IsZero() && time.Since(job.started) >= max(time.Second, 2*m.durations)
	default:
		return false
	}
}

// The caller holds m.mu. Lake submits only jobs whose imports have already finished.
func (m *master) schedule() {
	if m.draining {
		return
	}
	if m.snapshot != nil && m.ctx.Err() == nil {
		for _, worker := range m.workers {
			if worker.busy || worker.cache.seeding || worker.cache.seeded || worker.cache.seedFailed {
				continue
			}
			worker.busy, worker.cache.seeding = true, true
			m.running.Go(func() { m.seed(worker, *m.snapshot, m.sourceSubdir) })
		}
	}
	for m.ctx.Err() == nil {
		var chosen *pendingJob
		var worker *slot
		bestScore := 0.0
		var bestMissing int64
		for _, job := range m.pending {
			rank := float64(max(m.ranks[job.spec.job.Module], 1)) / float64(len(job.active)+1)
			if rank < bestScore {
				continue
			}
			for _, candidate := range m.workers {
				if candidate.busy || (m.sourceRoot != "" && !candidate.cache.seeded) || !m.canStart(job, candidate) {
					continue
				}
				var missing int64
				for _, file := range job.spec.job.Inputs {
					if !candidate.cache.known[file.Hash] {
						missing += file.Size
					}
				}
				score := rank
				if len(job.active) > 0 && missing != 0 {
					rate := candidate.cache.inputRate
					if rate == 0 {
						rate = 64 << 20
					}
					compileSeconds := max(200*time.Millisecond, m.durations).Seconds()
					score /= 1 + float64(missing)/rate/compileSeconds
				}
				if chosen == nil || score > bestScore || missing < bestMissing {
					chosen, worker, bestScore, bestMissing = job, candidate, score, missing
				}
			}
		}
		if chosen == nil {
			break
		}
		attempt, cancel := context.WithCancel(m.ctx)
		worker.busy = true
		chosen.active[worker] = cancel
		chosen.hosts[worker.host] = true
		chosen.attempts++
		m.next++
		id := m.next
		m.event("dispatch", chosen.spec.job.Module, worker.host, id)
		m.running.Go(func() { m.run(attempt, chosen, worker, id) })
	}
	for i := len(m.pending) - 1; i >= 0; i-- {
		job := m.pending[i]
		if len(job.active) != 0 {
			continue
		}
		available := !m.snapshotFailed && slices.ContainsFunc(m.workers, func(w *slot) bool {
			return !w.cache.seedFailed && m.canStart(job, w)
		})
		if available {
			continue
		}
		result := compileResult{Fallback: true}
		if job.compilerError != nil {
			result = *job.compilerError
		}
		m.finish(job, result)
	}
}

func (m *master) finish(job *pendingJob, result compileResult) {
	for _, cancel := range job.active {
		cancel()
	}
	m.pending = slices.DeleteFunc(m.pending, func(j *pendingJob) bool { return j == job })
	job.done <- result
}

func (m *master) run(ctx context.Context, job *pendingJob, worker *slot, id uint64) {
	start := time.Now()
	request := worker.w.request.Add(1)
	_ = worker.w.SetDeadline(start.Add(45 * time.Minute))
	sent, received := worker.w.sent.Load(), worker.w.received.Load()
	stop := context.AfterFunc(ctx, func() {
		_ = worker.w.SetDeadline(time.Now().Add(2 * time.Second))
		_ = worker.w.send(message{Type: "cancel", ID: request, Attempt: id})
	})
	var compileStart time.Time
	result, outputs, err := attempt(worker.w, request, id, job.spec, func() {
		compileStart = time.Now()
		inputTime := compileStart.Sub(start)
		inputBytes := worker.w.sent.Load() - sent
		m.mu.Lock()
		defer m.mu.Unlock()
		if job.started.IsZero() {
			job.started = compileStart
		}
		if inputBytes >= 64<<10 && inputTime > 0 {
			rate := float64(inputBytes) / inputTime.Seconds()
			if worker.cache.inputRate == 0 {
				worker.cache.inputRate = rate
			} else {
				worker.cache.inputRate = (7*worker.cache.inputRate + rate) / 8
			}
		}
		m.event("inputs-ready", job.spec.job.Module, worker.host, id, "wallNanos", inputTime.Nanoseconds())
		for _, entry := range job.spec.job.Inputs {
			worker.cache.known[entry.Hash] = true
		}
		m.schedule()
	})
	stop()
	if ctx.Err() == nil {
		_ = worker.w.SetDeadline(time.Time{})
	}
	defer func() {
		for _, output := range outputs {
			os.Remove(output.temp)
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(job.active, worker)
	if len(job.active) == 0 {
		job.started = time.Time{}
	}
	if err != nil || ctx.Err() != nil {
		worker.w.Close()
		m.workers = slices.DeleteFunc(m.workers, func(w *slot) bool { return w == worker })
	} else {
		worker.busy = false
	}
	m.event("attempt-end", job.spec.job.Module, worker.host, id, "cpuNanos", result.CPUNanos,
		"bytesSent", worker.w.sent.Load()-sent, "bytesReceived", worker.w.received.Load()-received,
		"cancelled", ctx.Err() != nil)
	if ctx.Err() != nil || !slices.Contains(m.pending, job) {
		m.schedule()
		return
	}
	if err == nil && result.Exit == 0 {
		err = publish(outputs)
		if err == nil {
			m.event("publish", job.spec.job.Module, worker.host, id)
			if m.durations == 0 {
				m.durations = time.Since(compileStart)
			} else {
				m.durations = (7*m.durations + time.Since(compileStart)) / 8
			}
			m.finish(job, result)
		}
	}
	if err != nil {
		debugf("attempt %d (%s on %s): %v", id, job.spec.job.Module, worker.host, err)
		if job.limit == 0 {
			job.limit = job.attempts + 1
		}
	} else if result.Exit != 0 {
		job.compilerError = &result
		job.limit = job.attempts
	}
	m.schedule()
}

func attempt(w *wire, id, attemptID uint64, p prepared, ready func()) (compileResult, []stagedOutput, error) {
	if err := w.send(message{Type: "job", ID: id, Attempt: attemptID, Job: &p.job}); err != nil {
		return compileResult{}, nil, err
	}
	msg, err := w.receive()
	if err != nil || msg.Type != "missing" || msg.ID != id {
		return compileResult{}, nil, errors.New("worker did not return input request")
	}
	inputs := map[string]fileEntry{}
	for _, entry := range p.job.Inputs {
		inputs[entry.Hash] = entry
	}
	for _, hash := range msg.Missing {
		entry, ok := inputs[hash]
		if !ok {
			return compileResult{}, nil, errors.New("worker requested unknown input")
		}
		delete(inputs, hash)
		if err := streamFile(w, id, p.paths[hash], entry); err != nil {
			return compileResult{}, nil, err
		}
	}
	msg, err = w.receive()
	if err != nil || msg.Type != "ready" || msg.ID != id {
		return compileResult{}, nil, errors.New("worker did not confirm verified inputs")
	}
	ready()
	msg, err = w.receive()
	if err != nil || msg.Type != "result" || msg.ID != id || msg.Attempt != attemptID || msg.Result == nil {
		return compileResult{}, nil, errors.New("worker did not return compiler result")
	}
	result := *msg.Result
	if result.Fallback {
		return result, nil, errors.New("worker cancelled the attempt")
	}
	if result.Exit != 0 {
		return result, nil, nil
	}
	expected := map[string]bool{}
	for _, path := range p.job.Outputs {
		expected[path] = true
	}
	var staged []stagedOutput
	for _, entry := range msg.Files {
		if !expected[entry.Path] || !validDigest(entry.Hash) || entry.Size < 0 {
			return result, staged, errors.New("invalid output manifest")
		}
		delete(expected, entry.Path)
		dest, err := materialPath("@work/"+strings.TrimPrefix(entry.Path, "@out/"), p.root, "", "")
		if err != nil {
			return result, staged, err
		}
		output, err := readOutput(w, id, entry, dest)
		if err != nil {
			return result, staged, err
		}
		staged = append(staged, output)
	}
	for _, path := range p.job.Required {
		if expected[path] {
			return result, staged, fmt.Errorf("worker did not return %s", path)
		}
	}
	return result, staged, nil
}

func readOutput(w *wire, id uint64, entry fileEntry, dest string) (stagedOutput, error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return stagedOutput{}, err
	}
	f, err := os.CreateTemp(dir, ".ocean-partial-")
	if err != nil {
		return stagedOutput{}, err
	}
	defer f.Close()
	ok := false
	defer func() {
		if !ok {
			os.Remove(f.Name())
		}
	}()
	var offset int64
	for {
		msg, err := w.receive()
		if err != nil || msg.Type != "chunk" || msg.ID != id || msg.Hash != entry.Hash {
			return stagedOutput{}, errors.New("incomplete output transfer")
		}
		done, err := writeChunk(f, msg, &offset, entry.Size)
		if err != nil {
			return stagedOutput{}, err
		}
		if done {
			break
		}
	}
	if err := f.Chmod(os.FileMode(entry.Mode) & 0777); err != nil {
		return stagedOutput{}, err
	}
	if err := f.Close(); err != nil {
		return stagedOutput{}, err
	}
	actual, err := describe(f.Name(), "")
	if err != nil || actual.Hash != entry.Hash || actual.Size != entry.Size {
		return stagedOutput{}, errors.New("output digest or size mismatch")
	}
	ok = true
	return stagedOutput{f.Name(), dest}, nil
}

func publish(outputs []stagedOutput) error {
	for i, output := range outputs {
		if err := os.Rename(output.temp, output.dest); err != nil {
			for _, written := range outputs[:i] {
				os.Remove(written.dest)
			}
			return err
		}
	}
	return nil
}
