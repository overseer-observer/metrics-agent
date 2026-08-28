// Package buffer stores on disk the samples that did not reach the backend.
//
// The format is line-delimited JSON (one sample per line) rotated over segment files.
// The simplicity is deliberate: the buffer must survive a SIGKILL in the middle of a write,
// and a corrupted last line must be skipped on read rather than crash the agent.
package buffer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"metrics-agent/internal/metrics"
)

const (
	// MaxTotalBytes is the hard cap of the buffer directory. On overflow the oldest
	// samples are evicted: fresh data is more valuable, and an agent on someone else's
	// server has no right to eat the disk.
	MaxTotalBytes = 32 << 20

	// MaxAge is the maximum age of a sample in the buffer (~7 days). The backend does not
	// compute thresholds from such samples anyway, yet they take up space.
	MaxAge = 7 * 24 * time.Hour

	// segmentBytes is the segment size after which a new file is started.
	segmentBytes = 1 << 20
)

const (
	segmentExt = ".jsonl"
	tempExt    = ".tmp"
)

// Buffer is a directory of segments. Not thread-safe: it is meant to be called
// from a single agent loop.
type Buffer struct {
	log *slog.Logger
	dir string

	// The caps live in fields so that tests do not push tens of megabytes through the disk.
	maxTotalBytes int64
	maxAge        time.Duration
	segmentBytes  int64
}

// Open creates the buffer directory (0700) and cleans up leftovers from the previous run.
func Open(log *slog.Logger, dir string) (*Buffer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create buffer directory %s: %w", dir, err)
	}
	b := &Buffer{
		log:           log,
		dir:           dir,
		maxTotalBytes: MaxTotalBytes,
		maxAge:        MaxAge,
		segmentBytes:  segmentBytes,
	}
	b.removeTemps()
	b.evict()
	return b, nil
}

// Add appends samples to the active segment. A write error is not fatal:
// losing a sample is worse than a crash, but not enough to bring the agent down.
func (b *Buffer) Add(samples []*metrics.Sample) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, s := range samples {
		if s == nil {
			continue
		}
		if err := enc.Encode(s); err != nil {
			b.log.Warn("failed to serialize a sample into the buffer", "err", err)
			return
		}
	}
	if buf.Len() == 0 {
		return
	}

	path := b.activeSegment(buf.Len())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		b.log.Error("failed to open a buffer segment", "path", path, "err", err)
		return
	}
	// A single write as one block: a torn write leaves a corrupted line but does not interleave samples.
	if _, err := f.Write(buf.Bytes()); err != nil {
		b.log.Error("failed to write to the buffer", "path", path, "err", err)
	}
	if err := f.Close(); err != nil {
		b.log.Error("failed to close a buffer segment", "path", path, "err", err)
	}
	b.log.Debug("samples stored in the buffer", "count", len(samples), "path", path)

	b.evict()
}

// Take returns up to limit of the oldest samples and a function that removes them from the buffer.
// Until commit is called the data stays on disk: a crash between the send and the
// removal leads to a repeated send, not to a loss. An empty buffer yields
// a nil slice and a nil function.
func (b *Buffer) Take(limit int) ([]*metrics.Sample, func(), error) {
	for _, name := range b.segments() {
		path := filepath.Join(b.dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read buffer segment %s: %w", path, err)
		}

		samples, consumed := b.decode(data, limit)
		if len(samples) == 0 {
			// The segment is empty or entirely unreadable, no point keeping it.
			b.remove(path)
			continue
		}
		return samples, func() { b.commit(path, consumed) }, nil
	}
	return nil, nil, nil
}

// decode parses line-delimited JSON, skipping corrupted lines. consumed is how many
// bytes the parsed and skipped lines occupy: exactly those are removed by commit.
func (b *Buffer) decode(data []byte, limit int) (samples []*metrics.Sample, consumed int) {
	for consumed < len(data) && len(samples) < limit {
		rest := data[consumed:]
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		consumed += len(line)

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var s metrics.Sample
		if err := json.Unmarshal(line, &s); err != nil {
			b.log.Warn("corrupted buffer line skipped", "err", err)
			continue
		}
		samples = append(samples, &s)
	}
	return samples, consumed
}

// commit removes the first consumed bytes of the segment. The file is re-read,
// so samples appended after Take are not lost. The rewrite goes through a
// temporary file and rename, so a crash mid-operation neither loses nor duplicates data.
func (b *Buffer) commit(path string, consumed int) {
	data, err := os.ReadFile(path)
	if err != nil {
		b.log.Warn("failed to re-read a buffer segment", "path", path, "err", err)
		return
	}
	if consumed >= len(data) {
		b.remove(path)
		return
	}

	tmp := path + tempExt
	if err := os.WriteFile(tmp, data[consumed:], 0o600); err != nil {
		b.log.Warn("failed to write a temporary segment", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		b.log.Warn("failed to replace a buffer segment", "path", path, "err", err)
		_ = os.Remove(tmp)
	}
}

// activeSegment returns the path of the segment to append n bytes to:
// the latest one, or a new one if that one outgrew segmentBytes.
func (b *Buffer) activeSegment(n int) string {
	names := b.segments()
	if len(names) > 0 {
		path := filepath.Join(b.dir, names[len(names)-1])
		if st, err := os.Stat(path); err == nil && st.Size()+int64(n) <= b.segmentBytes {
			return path
		}
	}
	// A name made of nanoseconds: lexicographic order matches chronological order.
	return filepath.Join(b.dir, fmt.Sprintf("%020d%s", time.Now().UnixNano(), segmentExt))
}

// segments returns the segment names from oldest to newest.
func (b *Buffer) segments() []string {
	// os.ReadDir returns entries sorted by name, that is, by creation time.
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.log.Warn("failed to read the buffer directory", "dir", b.dir, "err", err)
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), segmentExt) {
			names = append(names, e.Name())
		}
	}
	return names
}

// evict evicts the oldest segments until the buffer fits the size and age caps.
// The active (last) segment is left alone: it was just written to.
func (b *Buffer) evict() {
	names := b.segments()
	if len(names) < 2 {
		return
	}

	sizes := make([]int64, len(names))
	mtimes := make([]time.Time, len(names))
	var total int64
	for i, name := range names {
		st, err := os.Stat(filepath.Join(b.dir, name))
		if err != nil {
			continue
		}
		sizes[i] = st.Size()
		mtimes[i] = st.ModTime()
		total += st.Size()
	}

	deadline := time.Now().Add(-b.maxAge)
	for i := 0; i < len(names)-1; i++ {
		if total <= b.maxTotalBytes && !mtimes[i].Before(deadline) {
			return
		}
		path := filepath.Join(b.dir, names[i])
		b.remove(path)
		total -= sizes[i]
		b.log.Warn("old samples evicted from the buffer", "path", path, "bytes", sizes[i])
	}
}

// removeTemps cleans up temporary files left over from a crash in the middle of commit.
func (b *Buffer) removeTemps() {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), tempExt) {
			b.remove(filepath.Join(b.dir, e.Name()))
		}
	}
}

func (b *Buffer) remove(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		b.log.Warn("failed to remove a buffer file", "path", path, "err", err)
	}
}
