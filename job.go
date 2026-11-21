package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type invocation struct {
	Root     string
	CWD      string
	Module   string
	Source   string
	Setup    string
	Args     []string
	LeanPath []string
	Compiler string
}

type compileJob struct {
	Module            string
	CWD               string
	WorkspaceBelowCWD string
	Setup             []byte
	Args              []string
	LeanPath          []string
	Inputs            []fileEntry
	Outputs           []string
	Required          []string
}

type compileResult struct {
	Exit     int    `json:"exit"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Fallback bool   `json:"fallback,omitempty"`
	CPUNanos int64  `json:"cpuNanos,omitempty"`
}

type cachedFile struct {
	entry    fileEntry
	size     int64
	modified int64
}

type inputIndex struct {
	mu            sync.Mutex
	files         map[string]cachedFile
	snapshotOnce  sync.Once
	snapshotRoot  string
	snapshotFiles []fileEntry
	snapshotPaths map[string]string
	snapshotErr   error
}

func (index *inputIndex) snapshot(root string) ([]fileEntry, map[string]string, error) {
	index.snapshotOnce.Do(func() {
		index.snapshotRoot = root
		index.snapshotPaths = map[string]string{}
		add := func(name string) error {
			if !filepath.IsLocal(name) || strings.HasPrefix(name, ".lake/") {
				return nil
			}
			path := filepath.Join(root, name)
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil || !info.Mode().IsRegular() {
				return err
			}
			entry, err := index.file(path, "@work/"+filepath.ToSlash(name))
			if err != nil {
				return err
			}
			index.snapshotFiles = append(index.snapshotFiles, entry)
			index.snapshotPaths[entry.Hash] = path
			return nil
		}
		data, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", ".").Output()
		if err == nil && len(data) > 0 {
			for _, name := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
				if err := add(name); err != nil {
					index.snapshotErr = err
					return
				}
			}
		} else {
			index.snapshotErr = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() && path != root && strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				if entry.IsDir() || (!strings.HasSuffix(path, ".lean") && entry.Name() != "lean-toolchain" && entry.Name() != "lakefile.toml") {
					return nil
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				return add(rel)
			})
		}
	})
	if root != index.snapshotRoot {
		return nil, nil, errors.New("workspace changed during build")
	}
	return index.snapshotFiles, index.snapshotPaths, index.snapshotErr
}

func (index *inputIndex) file(path, logical string) (fileEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileEntry{}, err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	current := index.files[path]
	if current.entry.Hash != "" && current.size == info.Size() && current.modified == info.ModTime().UnixNano() {
		entry := current.entry
		entry.Path = logical
		return entry, nil
	}
	entry, err := describe(path, logical)
	if err == nil {
		index.files[path] = cachedFile{entry, info.Size(), info.ModTime().UnixNano()}
	}
	return entry, err
}

func inside(base, path string) (string, bool) {
	rel, err := filepath.Rel(base, path)
	return rel, err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func logicalPath(path, cwd, root, toolchain string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	for _, item := range []struct{ base, tag string }{{toolchain, "@tool"}, {root, "@work"}} {
		if rel, ok := inside(item.base, path); ok {
			return item.tag + "/" + filepath.ToSlash(rel), nil
		}
	}
	return "", fmt.Errorf("path is outside the workspace or toolchain: %s", path)
}

func materialPath(logical, work, toolchain, out string) (string, error) {
	tag, rest, ok := strings.Cut(logical, "/")
	if !ok || (rest != "." && !filepath.IsLocal(rest)) || filepath.Clean(rest) != rest {
		return "", fmt.Errorf("invalid logical path: %s", logical)
	}
	var root string
	switch tag {
	case "@work":
		root = work
	case "@tool":
		root = toolchain
	case "@out":
		root = out
	default:
		return "", fmt.Errorf("unknown path root: %s", tag)
	}
	if root == "" {
		return "", fmt.Errorf("unavailable path root: %s", tag)
	}
	return filepath.Join(root, rest), nil
}

func rewriteSetup(raw []byte, rewrite func(string) (string, error)) ([]byte, error) {
	var setup map[string]json.RawMessage
	if err := json.Unmarshal(raw, &setup); err != nil {
		return nil, err
	}
	for _, name := range []string{"importArts", "dynlibs", "plugins"} {
		raw, found := setup[name]
		if !found {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		var walk func(any) (any, error)
		walk = func(v any) (any, error) {
			switch v := v.(type) {
			case string:
				return rewrite(v)
			case []any:
				for i, item := range v {
					mapped, err := walk(item)
					if err != nil {
						return nil, err
					}
					v[i] = mapped
				}
			case map[string]any:
				for key, item := range v {
					if name == "plugins" && key != "path" {
						continue
					}
					mapped, err := walk(item)
					if err != nil {
						return nil, err
					}
					v[key] = mapped
				}
			case nil:
			default:
				return nil, fmt.Errorf("invalid %s entry", name)
			}
			return v, nil
		}
		value, err := walk(value)
		if err != nil {
			return nil, err
		}
		setup[name], err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(setup)
}

type prepared struct {
	job   compileJob
	root  string
	paths map[string]string
}

func prepare(v invocation, toolchain string, index *inputIndex) (prepared, error) {
	expected := filepath.Join(toolchain, "bin", "lean")
	if v.Compiler != expected {
		return prepared{}, fmt.Errorf("unexpected compiler %s", v.Compiler)
	}
	p := prepared{root: v.Root, paths: map[string]string{}}
	j := &p.job
	j.Module = v.Module
	var err error
	j.CWD, err = logicalPath(v.CWD, v.CWD, v.Root, toolchain)
	if err != nil {
		if rel, ok := inside(v.CWD, v.Root); ok && rel != "." {
			j.WorkspaceBelowCWD = rel
		} else {
			return p, err
		}
	}
	files := map[string]bool{}
	input := func(path string) (string, error) {
		logical, err := logicalPath(path, v.CWD, v.Root, toolchain)
		if err != nil || strings.HasPrefix(logical, "@tool/") || files[logical] {
			return logical, err
		}
		local, err := materialPath(logical, v.Root, toolchain, "")
		if err != nil {
			return "", err
		}
		entry, err := index.file(local, logical)
		if err != nil {
			return "", err
		}
		files[logical] = true
		j.Inputs = append(j.Inputs, entry)
		p.paths[entry.Hash] = local
		return logical, nil
	}
	for _, path := range v.LeanPath {
		logical, err := logicalPath(path, v.CWD, v.Root, toolchain)
		if err != nil {
			return p, err
		}
		j.LeanPath = append(j.LeanPath, logical)
	}
	setup, err := os.ReadFile(v.Setup)
	if err != nil {
		return p, err
	}
	j.Setup, err = rewriteSetup(setup, input)
	if err != nil {
		return p, err
	}
	source, err := input(v.Source)
	if err != nil {
		return p, err
	}
	j.Args = append([]string(nil), v.Args...)
	sawSource := false
	for i := 0; i < len(j.Args); i++ {
		arg := j.Args[i]
		switch arg {
		case "--setup":
			if i+1 == len(j.Args) || j.Args[i+1] != v.Setup {
				return p, errors.New("unexpected module setup argument")
			}
			i++
			j.Args[i] = "@setup"
		case "-o", "-i", "-c", "-b":
			if i+1 == len(j.Args) {
				return p, errors.New("missing compiler output path")
			}
			i++
			path, err := logicalPath(j.Args[i], v.CWD, v.Root, toolchain)
			if err != nil || !strings.HasPrefix(path, "@work/") {
				return p, errors.New("compiler output outside workspace")
			}
			output := "@out/" + strings.TrimPrefix(path, "@work/")
			j.Args[i] = output
			j.Outputs = append(j.Outputs, output)
			j.Required = append(j.Required, output)
			if arg == "-o" {
				stem := strings.TrimSuffix(output, ".olean")
				j.Outputs = append(j.Outputs, output+".server", output+".private", stem+".ir", stem+".ir.sig")
			}
		default:
			if arg == v.Source {
				j.Args[i] = source
				sawSource = true
			} else if strings.Contains(arg, "/") {
				return p, fmt.Errorf("unsupported path argument: %s", arg)
			}
		}
	}
	if !sawSource || len(j.Required) == 0 {
		return p, errors.New("not a module compilation")
	}
	return p, nil
}
