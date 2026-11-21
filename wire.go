package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/fxamacker/cbor/v2"
)

const (
	protocolVersion = 2
	maxFrame        = 32 << 20
	chunkSize       = 1 << 20
)

type fileEntry struct {
	Path string
	Hash string
	Size int64
	Mode uint32
}

type message struct {
	Type      string
	ID        uint64
	Attempt   uint64
	Version   int
	Host      string
	Platform  string
	Toolchain string
	Slots     int
	Job       *compileJob
	Graph     map[string][]string
	Root      string
	CWD       string
	Files     []fileEntry
	Missing   []string
	Hash      string
	Offset    int64
	Data      []byte
	Result    *compileResult
	Error     string
}

type wire struct {
	net.Conn
	in       *bufio.Reader
	mu       sync.Mutex
	sent     atomic.Int64
	received atomic.Int64
	request  atomic.Uint64
}

func newWire(conn net.Conn) *wire {
	return &wire{Conn: conn, in: bufio.NewReader(conn)}
}

var decodeMode = func() cbor.DecMode {
	mode, err := (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF,
		MaxNestedLevels: 32, MaxArrayElements: 131072, MaxMapPairs: 131072}).DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func (w *wire) send(msg message) error {
	data, err := cbor.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > maxFrame {
		return fmt.Errorf("message exceeds %d bytes", maxFrame)
	}
	frame := make([]byte, 4, len(data)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	frame = append(frame, data...)
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.Write(frame)
	w.sent.Add(int64(n))
	return err
}

func (w *wire) receive() (message, error) {
	var header [4]byte
	n, err := io.ReadFull(w.in, header[:])
	w.received.Add(int64(n))
	if err != nil {
		return message{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrame {
		return message{}, fmt.Errorf("invalid message size: %d", size)
	}
	var msg message
	data := make([]byte, size)
	n, err = io.ReadFull(w.in, data)
	w.received.Add(int64(n))
	if err != nil {
		return msg, err
	}
	return msg, decodeMode.Unmarshal(data, &msg)
}

func streamFile(w *wire, id uint64, name string, entry fileEntry) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, chunkSize)
	for offset := int64(0); offset < entry.Size; {
		n, err := io.ReadFull(f, buf[:min(int64(len(buf)), entry.Size-offset)])
		if err != nil {
			return err
		}
		if err := w.send(message{Type: "chunk", ID: id, Hash: entry.Hash, Offset: offset, Data: buf[:n]}); err != nil {
			return err
		}
		offset += int64(n)
	}
	return w.send(message{Type: "chunk", ID: id, Hash: entry.Hash, Offset: entry.Size})
}

func describe(path, logical string) (fileEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileEntry{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return fileEntry{}, fmt.Errorf("not a regular file: %s", path)
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return fileEntry{}, err
	}
	return fileEntry{logical, hex.EncodeToString(h.Sum(nil)), size, uint32(info.Mode().Perm())}, nil
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func writeChunk(f *os.File, msg message, offset *int64, size int64) (bool, error) {
	if msg.Offset != *offset || len(msg.Data) > chunkSize || int64(len(msg.Data)) > size-*offset {
		return false, errors.New("invalid chunk offset or size")
	}
	if len(msg.Data) == 0 {
		if *offset != size {
			return false, errors.New("premature end of transfer")
		}
		return true, nil
	}
	n, err := f.Write(msg.Data)
	*offset += int64(n)
	return false, err
}
