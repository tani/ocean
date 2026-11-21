package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourcePack(t *testing.T) {
	local := t.TempDir()
	input := filepath.Join(local, "source.txt")
	const contents = "input read by Lean during compilation\n"
	if err := os.WriteFile(input, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	entry, err := describe(input, "@work/resources/path with spaces.txt")
	if err != nil {
		t.Fatal(err)
	}
	pack, err := packSources(local, []fileEntry{entry}, map[string]string{entry.Hash: input})
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := unpackSources(pack.path, stage, pack.files); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "resources", "path with spaces.txt"))
	if err != nil || string(data) != contents {
		t.Fatalf("unpacked source %q, err %v", data, err)
	}
	pack.files[0].Hash = "invalid digest"
	if err := unpackSources(pack.path, t.TempDir(), pack.files); err == nil {
		t.Fatal("accepted a source with a mismatched digest")
	}
}
