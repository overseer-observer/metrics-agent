package metrics

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestCollector builds a collector on top of the dumps in testdata:
// the real /proc and the real statfs are not used in tests.
func newTestCollector(t *testing.T, root string) (*Collector, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	c := &Collector{
		log:            slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		root:           root,
		statfs:         func(string) (usage, error) { return usage{Total: 1, InodesTotal: 1}, nil },
		statfsTimeout:  time.Second,
		hostname:       func() (string, error) { return "web-01", nil },
		maxFilesystems: DefaultMaxFilesystems,
		logged:         make(map[string]bool),
	}
	base := time.Unix(1755000000, 0)
	step := 0
	c.now = func() time.Time {
		now := base.Add(time.Duration(step) * time.Minute)
		step++
		return now
	}
	return c, &buf
}

func TestCollectSkipsFirstSample(t *testing.T) {
	c, _ := newTestCollector(t, "testdata/base")
	if s := c.Collect(context.Background()); s != nil {
		t.Fatalf("the first sample must be skipped, got %+v", s)
	}
}

func TestCollectCPUDelta(t *testing.T) {
	c, _ := newTestCollector(t, "testdata/base")
	if s := c.Collect(context.Background()); s != nil {
		t.Fatalf("the first sample must be skipped")
	}
	c.root = "testdata/next"
	s := c.Collect(context.Background())
	if s == nil {
		t.Fatal("the second sample must not be skipped")
	}

	want := CPU{Cores: 2, User: 11.36, System: 5.68, IOWait: 2.27, Steal: 1.14, Idle: 79.55}
	if s.CPU != want {
		t.Errorf("cpu = %+v, want %+v", s.CPU, want)
	}
	sum := s.CPU.User + s.CPU.System + s.CPU.IOWait + s.CPU.Steal + s.CPU.Idle
	if sum < 99.5 || sum > 100.5 {
		t.Errorf("the sum of the cpu components = %v, want about 100", sum)
	}

	if s.TS != 1755000060 {
		t.Errorf("ts = %d, want 1755000060", s.TS)
	}
	if s.Uptime != 864000 {
		t.Errorf("uptime = %d, want 864000", s.Uptime)
	}
	if s.LA != [3]float64{0.8, 1.2, 1.1} {
		t.Errorf("la = %v", s.LA)
	}

	wantMem := Mem{
		Total: 16777216000, Free: 1048576000, Available: 4194304000,
		Buffers: 268435456, Cached: 2147483648, Shmem: 67108864,
	}
	if s.Mem != wantMem {
		t.Errorf("mem = %+v, want %+v", s.Mem, wantMem)
	}

	// pswpin 100 -> 130 and pswpout 200 -> 260 over 60 seconds.
	wantSwap := Swap{Total: 2147483648, Used: 104857600, InRate: 0.5, OutRate: 1}
	if s.Swap != wantSwap {
		t.Errorf("swap = %+v, want %+v", s.Swap, wantSwap)
	}
}

func TestCollectSkipsOnCounterReset(t *testing.T) {
	c, buf := newTestCollector(t, "testdata/base")
	c.Collect(context.Background())
	c.root = "testdata/next"
	if c.Collect(context.Background()) == nil {
		t.Fatal("a sample after the baseline must be returned")
	}

	c.root = "testdata/reset"
	if s := c.Collect(context.Background()); s != nil {
		t.Fatalf("a sample with counters that went backwards must be skipped, got %+v", s)
	}
	if !strings.Contains(buf.String(), "CPU counters were reset") {
		t.Error("the counter reset was not logged")
	}

	// The baseline was reset to the reset dump, the next sample is correct again.
	c.root = "testdata/next"
	s := c.Collect(context.Background())
	if s == nil {
		t.Fatal("a sample after the baseline reset must be returned")
	}
	sum := s.CPU.User + s.CPU.System + s.CPU.IOWait + s.CPU.Steal + s.CPU.Idle
	if sum < 99.5 || sum > 100.5 {
		t.Errorf("the sum of the cpu components = %v, want about 100", sum)
	}
	if s.CPU.User < 0 || s.CPU.Idle < 0 {
		t.Errorf("negative percentages: %+v", s.CPU)
	}
}

func TestCollectSurvivesUnreadableVmstat(t *testing.T) {
	c, buf := newTestCollector(t, "testdata/novmstat")
	for i := 0; i < 3; i++ {
		c.Collect(context.Background())
	}

	if n := strings.Count(buf.String(), "failed to read /proc/vmstat"); n != 1 {
		t.Errorf("the vmstat error was logged %d times, want 1", n)
	}
}

func TestCollectWithoutVmstatKeepsSample(t *testing.T) {
	c, _ := newTestCollector(t, "testdata/novmstat")
	c.Collect(context.Background())

	// A second dump with grown CPU counters, but still without vmstat.
	root := t.TempDir()
	copyTree(t, "testdata/next", root)
	if err := os.Remove(filepath.Join(root, "proc/vmstat")); err != nil {
		t.Fatal(err)
	}
	c.root = root

	s := c.Collect(context.Background())
	if s == nil {
		t.Fatal("an unreadable vmstat must not break the sample")
	}
	if s.Swap.InRate != 0 || s.Swap.OutRate != 0 {
		t.Errorf("without vmstat the swap rates must be zero, got %+v", s.Swap)
	}
	if s.Swap.Total == 0 {
		t.Error("swap volumes are read from meminfo and must be populated")
	}
}

func TestHost(t *testing.T) {
	c, _ := newTestCollector(t, "testdata/base")
	h := c.Host()
	want := Host{
		Hostname: "web-01",
		OS:       "Ubuntu 22.04.4 LTS",
		BootID:   "9f2c1f8a-0000-4000-8000-000000000001",
	}
	if h != want {
		t.Errorf("host = %+v, want %+v", h, want)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
