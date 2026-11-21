package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type workerState struct {
	root      string
	toolchain string
	mu        sync.Mutex
	installed map[string]string
	transfers map[string]*blobTransfer
}

type blobTransfer struct {
	done chan struct{}
	err  error
}

func workerMain(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("flake-worker", flag.ContinueOnError)
	address := flags.String("connect", "", "master TCP address")
	hostname, _ := os.Hostname()
	host := flags.String("host", hostname, "identity shared by all slots on this host")
	slots := flags.Int("slots", max(1, runtime.GOMAXPROCS(0)-1), "compiler slots")
	root := flags.String("toolchain", "", "path to the Lean 4.35.0 toolchain")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *address == "" || *host == "" || *slots < 1 || flags.NArg() != 0 {
		return errors.New("usage: flake-worker --connect HOST:PORT [--host ID] [--slots N] [--toolchain PATH]")
	}
	toolchain := *root
	if toolchain == "" {
		var err error
		toolchain, err = commandText(ctx, "lean", "--print-prefix")
		if err != nil {
			return err
		}
	}
	toolchain, err := filepath.Abs(toolchain)
	if err != nil {
		return err
	}
	fingerprint, err := commandText(ctx, filepath.Join(toolchain, "bin", "lean"), "--githash")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "flake-worker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	state := &workerState{root: dir, toolchain: toolchain, installed: map[string]string{}, transfers: map[string]*blobTransfer{}}
	cacheID := sha256.Sum256([]byte(dir))
	var running sync.WaitGroup
	for range *slots {
		running.Go(func() {
			hello := message{Type: "register", Version: protocolVersion, Host: *host, Platform: platform(), Toolchain: fingerprint, Slots: *slots}
			hello.Hash = hex.EncodeToString(cacheID[:])
			for ctx.Err() == nil {
				conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", *address)
				if err == nil {
					err = state.serve(ctx, newWire(conn), hello)
					conn.Close()
					if err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
						debugf("worker connection: %v", err)
					}
				}
				select {
				case <-ctx.Done():
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
	running.Wait()
	return nil
}

func (s *workerState) serve(parent context.Context, w *wire, hello message) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { w.Close() })
	defer stop()
	if err := w.send(hello); err != nil {
		return err
	}
	ack, err := w.receive()
	if err != nil {
		return err
	}
	if ack.Type != "registered" || !validDigest(ack.Hash) {
		return fmt.Errorf("registration rejected: %s", ack.Error)
	}
	session := filepath.Join(s.root, ack.Hash)
	if err := os.MkdirAll(filepath.Join(session, "blobs"), 0700); err != nil {
		return err
	}
	input := make(chan message, 2)
	var attemptMu sync.Mutex
	var currentID uint64
	var currentCancel context.CancelFunc
	var cancelNext uint64
	go func() {
		defer close(input)
		defer cancel()
		for {
			msg, err := w.receive()
			if err != nil {
				return
			}
			if msg.Type == "cancel" {
				attemptMu.Lock()
				if msg.ID == currentID && currentCancel != nil {
					currentCancel()
				} else if msg.ID > currentID {
					cancelNext = msg.ID
				}
				attemptMu.Unlock()
				continue
			}
			select {
			case input <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	for msg := range input {
		if msg.Type == "seed" {
			if err := s.seed(w, session, msg, input); err != nil {
				return err
			}
			continue
		}
		if msg.Type != "job" || msg.Job == nil {
			return errors.New("expected job")
		}
		attemptCtx, stopAttempt := context.WithCancel(ctx)
		attemptMu.Lock()
		currentID, currentCancel = msg.ID, stopAttempt
		if cancelNext == msg.ID {
			stopAttempt()
		}
		attemptMu.Unlock()
		err := s.run(attemptCtx, w, session, msg, input)
		stopAttempt()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func workerWorkspace(tree, rel string) (string, error) {
	if rel == "" {
		return tree, nil
	}
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
		return "", errors.New("invalid workspace location")
	}
	return filepath.Join(tree, rel), nil
}

func (s *workerState) seed(w *wire, session string, msg message, input <-chan message) error {
	if len(msg.Files) == 0 || !validDigest(msg.Files[0].Hash) || msg.Files[0].Size < 0 {
		return errors.New("invalid source archive")
	}
	work, err := workerWorkspace(filepath.Join(session, "tree"), msg.CWD)
	if err != nil {
		return err
	}
	pack, files := msg.Files[0], msg.Files[1:]
	blobs := filepath.Join(session, "blobs")
	if err := receiveBlob(blobs, msg.ID, pack, input); err != nil {
		return err
	}
	packPath := filepath.Join(blobs, pack.Hash)
	defer os.Remove(packPath)
	if err := os.MkdirAll(filepath.Dir(work), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(session, "sources-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := unpackSources(packPath, stage, files); err != nil {
		return err
	}
	if err := os.Rename(stage, work); err != nil {
		return err
	}
	s.mu.Lock()
	for _, entry := range files {
		path := filepath.Join(work, strings.TrimPrefix(entry.Path, "@work/"))
		s.installed[path] = entry.Hash
	}
	s.mu.Unlock()
	return w.send(message{Type: "seeded", ID: msg.ID})
}

func (s *workerState) receiveInputs(ctx context.Context, w *wire, work, blobs string, msg message, input <-chan message) (err error) {
	missing := map[string]fileEntry{}
	var requested []string
	waiting := map[string]*blobTransfer{}
	owned := map[string]*blobTransfer{}
	resolve := func(hash string, result error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		flight := owned[hash]
		flight.err = result
		close(flight.done)
		delete(s.transfers, filepath.Join(blobs, hash))
		delete(owned, hash)
	}
	defer func() {
		for hash := range owned {
			resolve(hash, err)
		}
	}()
	for _, entry := range msg.Job.Inputs {
		if !validDigest(entry.Hash) || entry.Size < 0 || !strings.HasPrefix(entry.Path, "@work/") {
			return errors.New("invalid input manifest")
		}
		path, err := materialPath(entry.Path, work, "", "")
		if err != nil {
			return err
		}
		blob := filepath.Join(blobs, entry.Hash)
		s.mu.Lock()
		if s.installed[path] == entry.Hash {
			s.mu.Unlock()
			continue
		}
		if _, err := os.Stat(blob); errors.Is(err, os.ErrNotExist) {
			if flight := s.transfers[blob]; flight == nil {
				flight = &blobTransfer{done: make(chan struct{})}
				s.transfers[blob], owned[entry.Hash] = flight, flight
				requested = append(requested, entry.Hash)
				missing[entry.Hash] = entry
			} else if owned[entry.Hash] == nil {
				waiting[entry.Hash] = flight
			}
		} else if err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
	}
	if err := w.send(message{Type: "missing", ID: msg.ID, Missing: requested}); err != nil {
		return err
	}
	for _, digest := range requested {
		entry := missing[digest]
		if err := receiveBlob(blobs, msg.ID, entry, input); err != nil {
			return err
		}
		resolve(digest, nil)
	}
	for _, flight := range waiting {
		select {
		case <-flight.done:
			if flight.err != nil {
				return flight.err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range msg.Job.Inputs {
		path, err := materialPath(entry.Path, work, "", "")
		if err != nil {
			return err
		}
		if old := s.installed[path]; old != "" {
			if old != entry.Hash {
				return fmt.Errorf("input changed during build: %s", entry.Path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(blobs, entry.Hash), path); err != nil {
			return err
		}
		s.installed[path] = entry.Hash
	}
	return nil
}

func receiveBlob(dir string, id uint64, entry fileEntry, input <-chan message) error {
	f, err := os.CreateTemp(dir, ".partial-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	var offset int64
	for {
		msg, ok := <-input
		if !ok || msg.ID != id || msg.Type != "chunk" || msg.Hash != entry.Hash {
			return errors.New("incomplete input transfer")
		}
		done, err := writeChunk(f, msg, &offset, entry.Size)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	actual, err := describe(f.Name(), "")
	if err != nil || actual.Hash != entry.Hash || actual.Size != entry.Size {
		return errors.New("input digest or size mismatch")
	}
	if err := os.Chmod(f.Name(), 0444); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, entry.Hash))
}

func (s *workerState) run(ctx context.Context, w *wire, session string, msg message, input <-chan message) error {
	tree := filepath.Join(session, "tree")
	work, err := workerWorkspace(tree, msg.Job.WorkspaceBelowCWD)
	if err != nil {
		return err
	}
	if err := s.receiveInputs(ctx, w, work, filepath.Join(session, "blobs"), msg, input); err != nil {
		if ctx.Err() != nil {
			return w.send(message{Type: "result", ID: msg.ID, Attempt: msg.Attempt, Result: &compileResult{Fallback: true}})
		}
		return err
	}
	if err := w.send(message{Type: "ready", ID: msg.ID}); err != nil {
		return err
	}
	out, err := os.MkdirTemp(s.root, "attempt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(out)
	result, files, err := s.compile(ctx, msg.Job, tree, work, out)
	if err != nil {
		if ctx.Err() != nil {
			result.Fallback = true
			return w.send(message{Type: "result", ID: msg.ID, Attempt: msg.Attempt, Result: &result})
		}
		return err
	}
	if err := w.send(message{Type: "result", ID: msg.ID, Attempt: msg.Attempt, Result: &result, Files: files}); err != nil {
		return err
	}
	for _, entry := range files {
		path, err := materialPath(entry.Path, work, s.toolchain, out)
		if err != nil {
			return err
		}
		if err := streamFile(w, msg.ID, path, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *workerState) compile(ctx context.Context, job *compileJob, session, work, out string) (compileResult, []fileEntry, error) {
	path := func(p string) (string, error) { return materialPath(p, work, s.toolchain, out) }
	setup, err := rewriteSetup(job.Setup, path)
	if err != nil {
		return compileResult{}, nil, err
	}
	setupFile := filepath.Join(out, "setup.json")
	if err := os.WriteFile(setupFile, setup, 0600); err != nil {
		return compileResult{}, nil, err
	}
	args := append([]string(nil), job.Args...)
	for i, arg := range args {
		if arg == "@setup" {
			args[i] = setupFile
		} else if strings.HasPrefix(arg, "@") {
			args[i], err = path(arg)
			if err != nil {
				return compileResult{}, nil, err
			}
		}
	}
	for _, output := range job.Outputs {
		p, err := path(output)
		if err != nil {
			return compileResult{}, nil, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return compileResult{}, nil, err
		}
	}
	var leanPath []string
	for _, logical := range job.LeanPath {
		p, err := path(logical)
		if err != nil {
			return compileResult{}, nil, err
		}
		leanPath = append(leanPath, p)
	}
	cwd := session
	if job.CWD != "" {
		cwd, err = path(job.CWD)
		if err != nil {
			return compileResult{}, nil, err
		}
	}
	cmd := exec.CommandContext(ctx, filepath.Join(s.toolchain, "bin", "lean"), args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "LEAN_SYSROOT="+s.toolchain, "LEAN_PATH="+strings.Join(leanPath, string(os.PathListSeparator)), "LEAN_NUM_THREADS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	result := compileResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		result.CPUNanos = int64(cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime())
	}
	if err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() < 0 {
			return result, nil, err
		}
		result.Exit = exit.ExitCode()
		return result, nil, nil
	}
	var files []fileEntry
	for _, output := range job.Outputs {
		p, err := path(output)
		if err != nil {
			return result, nil, err
		}
		entry, err := describe(p, output)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, nil, err
		}
		files = append(files, entry)
	}
	return result, files, nil
}
