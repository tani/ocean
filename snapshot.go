package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type sourcePack struct {
	entry fileEntry
	path  string
	files []fileEntry
}

func packSources(dir string, files []fileEntry, paths map[string]string) (sourcePack, error) {
	file, err := os.CreateTemp(dir, "sources-*.tar.gz")
	if err != nil {
		return sourcePack{}, err
	}
	defer file.Close()
	ok := false
	defer func() {
		if !ok {
			os.Remove(file.Name())
		}
	}()
	compressed, _ := gzip.NewWriterLevel(file, gzip.BestSpeed)
	archive := tar.NewWriter(compressed)
	for _, entry := range files {
		if err := addSource(archive, entry, paths[entry.Hash]); err != nil {
			return sourcePack{}, err
		}
	}
	if err := archive.Close(); err != nil {
		return sourcePack{}, err
	}
	if err := compressed.Close(); err != nil {
		return sourcePack{}, err
	}
	if err := file.Close(); err != nil {
		return sourcePack{}, err
	}
	entry, err := describe(file.Name(), "")
	if err != nil {
		return sourcePack{}, err
	}
	ok = true
	return sourcePack{entry: entry, path: file.Name(), files: files}, nil
}

func addSource(archive *tar.Writer, entry fileEntry, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := archive.WriteHeader(&tar.Header{
		Name: strings.TrimPrefix(entry.Path, "@work/"), Typeflag: tar.TypeReg,
		Mode: int64(entry.Mode), Size: entry.Size,
	}); err != nil {
		return err
	}
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, digest), file); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != entry.Hash {
		return fmt.Errorf("source changed during snapshot: %s", entry.Path)
	}
	return nil
}

func unpackSources(archivePath, stage string, files []fileEntry) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	for _, entry := range files {
		header, err := archive.Next()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(entry.Path, "@work/") || header.Typeflag != tar.TypeReg ||
			header.Name != strings.TrimPrefix(entry.Path, "@work/") || header.Size != entry.Size {
			return fmt.Errorf("unexpected source archive member: %q", header.Name)
		}
		dest, err := materialPath(entry.Path, stage, "", "")
		if err != nil {
			return err
		}
		if err := writeSource(archive, dest, entry); err != nil {
			return err
		}
	}
	if _, err := archive.Next(); err != io.EOF {
		return fmt.Errorf("unexpected data after source archive: %v", err)
	}
	// Read through the gzip trailer to surface compression errors.
	_, err = io.Copy(io.Discard, compressed)
	return err
}

func writeSource(archive io.Reader, dest string, entry fileEntry) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, copyErr := io.CopyN(io.MultiWriter(file, digest), archive, entry.Size)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(digest.Sum(nil)) != entry.Hash {
		return fmt.Errorf("source digest mismatch: %s", entry.Path)
	}
	return nil
}
