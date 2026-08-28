package buffer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"metrics-agent/internal/logging"
	"metrics-agent/internal/metrics"
)

func newBuffer(t *testing.T) *Buffer {
	t.Helper()
	b, err := Open(logging.New("debug", &bytes.Buffer{}), filepath.Join(t.TempDir(), "buffer"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b
}

func samples(ts ...int64) []*metrics.Sample {
	out := make([]*metrics.Sample, 0, len(ts))
	for _, v := range ts {
		out = append(out, &metrics.Sample{TS: v})
	}
	return out
}

func timestamps(in []*metrics.Sample) []int64 {
	out := make([]int64, 0, len(in))
	for _, s := range in {
		out = append(out, s.TS)
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAddTakeCommit(t *testing.T) {
	b := newBuffer(t)
	b.Add(samples(1, 2))
	b.Add(samples(3))

	got, commit, err := b.Take(10)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !equal(timestamps(got), []int64{1, 2, 3}) {
		t.Fatalf("sample order is broken: %v", timestamps(got))
	}
	commit()

	got, _, err = b.Take(10)
	if err != nil {
		t.Fatalf("Take after commit: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the buffer is not empty after commit: %v", timestamps(got))
	}
}

func TestTakeRespectsLimitAndKeepsRest(t *testing.T) {
	b := newBuffer(t)
	b.Add(samples(1, 2, 3, 4, 5))

	got, commit, _ := b.Take(2)
	if !equal(timestamps(got), []int64{1, 2}) {
		t.Fatalf("the limit was not respected: %v", timestamps(got))
	}
	commit()

	got, _, _ = b.Take(10)
	if !equal(timestamps(got), []int64{3, 4, 5}) {
		t.Fatalf("the tail of the buffer is lost: %v", timestamps(got))
	}
}

// A crash between sending a batch and removing it leads to a repeated send,
// not to a loss: without commit the data stays on disk.
func TestTakeWithoutCommitKeepsData(t *testing.T) {
	b := newBuffer(t)
	b.Add(samples(1, 2))

	if got, _, _ := b.Take(10); len(got) != 2 {
		t.Fatalf("the first Take returned %d samples", len(got))
	}
	got, _, _ := b.Take(10)
	if !equal(timestamps(got), []int64{1, 2}) {
		t.Fatalf("data disappeared without commit: %v", timestamps(got))
	}
}

// A SIGKILL in the middle of a write leaves an unfinished line: it is skipped,
// the rest are read.
func TestBrokenLineSkipped(t *testing.T) {
	b := newBuffer(t)
	b.Add(samples(1, 2))

	names := b.segments()
	path := filepath.Join(b.dir, names[0])
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if _, err := f.WriteString(`{"ts":3,"upt`); err != nil {
		t.Fatalf("append the fragment: %v", err)
	}
	f.Close()

	got, commit, err := b.Take(10)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !equal(timestamps(got), []int64{1, 2}) {
		t.Fatalf("a corrupted line broke the read: %v", timestamps(got))
	}
	commit()

	// The fragment goes away too: there is no one left to finish writing it.
	if got, _, _ := b.Take(10); len(got) != 0 {
		t.Fatalf("%d samples remain after commit", len(got))
	}
}

func TestEvictOldestOnOverflow(t *testing.T) {
	b := newBuffer(t)
	b.segmentBytes = 200
	b.maxTotalBytes = 600

	for i := int64(1); i <= 40; i++ {
		b.Add(samples(i))
	}

	var total int64
	for _, name := range b.segments() {
		st, err := os.Stat(filepath.Join(b.dir, name))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		total += st.Size()
	}
	// One segment over the cap is acceptable: the active one is not evicted.
	if total > b.maxTotalBytes+b.segmentBytes {
		t.Fatalf("the buffer grew to %d bytes with a cap of %d", total, b.maxTotalBytes)
	}

	got, _, _ := b.Take(100)
	if len(got) == 0 {
		t.Fatal("the buffer is empty after eviction")
	}
	// The oldest ones are evicted: fresh data is more valuable.
	if got[len(got)-1].TS < got[0].TS {
		t.Fatalf("the order is broken: %v", timestamps(got))
	}
	if got[0].TS == 1 {
		t.Fatalf("old samples were not evicted: %v", timestamps(got))
	}
}

func TestEvictByAge(t *testing.T) {
	b := newBuffer(t)
	b.segmentBytes = 1
	b.Add(samples(1))
	b.Add(samples(2))

	old := filepath.Join(b.dir, b.segments()[0])
	stale := time.Now().Add(-MaxAge - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	b.evict()

	got, _, _ := b.Take(10)
	if !equal(timestamps(got), []int64{2}) {
		t.Fatalf("the stale segment was not removed: %v", timestamps(got))
	}
}

func TestPermissions(t *testing.T) {
	b := newBuffer(t)
	b.Add(samples(1))

	st, err := os.Stat(b.dir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", st.Mode().Perm())
	}
	st, err = os.Stat(filepath.Join(b.dir, b.segments()[0]))
	if err != nil {
		t.Fatalf("stat segment: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("segment mode %v", st.Mode().Perm())
	}
}

func TestTempFilesRemovedOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "buffer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	leftover := filepath.Join(dir, "00000000000000000001.jsonl.tmp")
	if err := os.WriteFile(leftover, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Open(logging.New("debug", &bytes.Buffer{}), dir); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("the temporary file was not removed: %v", err)
	}
}
