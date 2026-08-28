package metrics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxFilesystems is the hard limit on the number of filesystems in a sample (plan item 4.4).
const DefaultMaxFilesystems = 32

// DefaultStatfsTimeout is how long we wait for statfs on a single mount point
// before declaring it hung and dropping it from the sample.
const DefaultStatfsTimeout = 2 * time.Second

// Collector collects samples. It keeps the previous counter values
// so that it reports percentages and rates instead of monotonic counters (plan item 4.1).
//
// Collect is not thread-safe: it is meant to be called from a single agent loop.
type Collector struct {
	log *slog.Logger

	// root is the filesystem root. In tests it is replaced with a directory of dumps.
	root string
	// statfs, now and hostname are replaced in tests.
	statfs        statfsFunc
	statfsTimeout time.Duration
	now           func() time.Time
	hostname      func() (string, error)

	maxFilesystems int

	prev   *counters
	logged map[string]bool

	// cores is read once: the number of cores does not change during the process lifetime.
	coresOnce int
}

// counters holds the monotonic counters of the previous sample and the moment they were taken.
type counters struct {
	at  time.Time
	cpu cpuTimes
	// swap is not always known: /proc/vmstat may be unreadable.
	swapKnown bool
	swapIn    uint64
	swapOut   uint64
}

// cpuTimes holds the ticks from /proc/stat, already folded into the contract fields.
type cpuTimes struct {
	user   uint64
	system uint64
	iowait uint64
	steal  uint64
	idle   uint64
	total  uint64
}

// New creates a collector that reads the real filesystem.
func New(log *slog.Logger) *Collector {
	return &Collector{
		log:            log,
		root:           "/",
		statfs:         statfs,
		statfsTimeout:  DefaultStatfsTimeout,
		now:            time.Now,
		hostname:       os.Hostname,
		maxFilesystems: DefaultMaxFilesystems,
		logged:         make(map[string]bool),
	}
}

// Host returns the immutable machine characteristics.
// An unreadable source yields an empty field, not an error.
func (c *Collector) Host() Host {
	h := Host{}
	if name, err := c.hostname(); err != nil {
		c.warnOnce("hostname", "failed to determine the hostname", err)
	} else {
		h.Hostname = name
	}
	if raw, err := c.readOnce("boot_id", "proc/sys/kernel/random/boot_id", "failed to read boot_id"); err == nil {
		h.BootID = strings.TrimSpace(string(raw))
	}
	if raw, err := c.readOnce("os-release", "etc/os-release", "failed to read os-release"); err == nil {
		h.OS = prettyName(raw)
	}
	return h
}

// Collect takes a sample. Returns nil if the sample is skipped: there is no baseline
// for deltas yet (the first call after start) or the counters were reset (a reboot,
// an overflow). In both cases the baseline is reset to the current values.
func (c *Collector) Collect(ctx context.Context) *Sample {
	now := c.now()

	cur := &counters{at: now}
	cpu, cpuErr := c.readCPUTimes()
	if cpuErr == nil {
		cur.cpu = cpu
	}
	if in, out, err := c.readSwapCounters(); err == nil {
		cur.swapKnown, cur.swapIn, cur.swapOut = true, in, out
	}

	prev := c.prev
	c.prev = cur

	if cpuErr != nil {
		// Without /proc/stat the delta cannot be computed, the sample is meaningless.
		return nil
	}
	if prev == nil {
		c.log.Debug("first sample skipped: no baseline for deltas")
		return nil
	}
	elapsed := now.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return nil
	}
	cpuPct, ok := cpuPercent(prev.cpu, cur.cpu)
	if !ok {
		c.log.Warn("CPU counters were reset, sample skipped, baseline reset")
		return nil
	}

	s := &Sample{TS: now.Unix(), CPU: cpuPct}
	s.CPU.Cores = c.cores()
	s.Uptime = c.uptime()
	s.LA = c.loadavg()
	s.Mem, s.Swap = c.memory()
	s.Swap.InRate, s.Swap.OutRate = swapRates(prev, cur, elapsed)
	s.FS = c.filesystems(ctx)
	return s
}

// cpuPercent converts a tick delta into percentages. The second value is false
// if the counters did not grow: that is what a reboot or an overflow looks like.
func cpuPercent(prev, cur cpuTimes) (CPU, bool) {
	if cur.total <= prev.total ||
		cur.user < prev.user || cur.system < prev.system ||
		cur.iowait < prev.iowait || cur.steal < prev.steal || cur.idle < prev.idle {
		return CPU{}, false
	}
	total := float64(cur.total - prev.total)
	pct := func(a, b uint64) float64 {
		return round2(float64(b-a) / total * 100)
	}
	return CPU{
		User:   pct(prev.user, cur.user),
		System: pct(prev.system, cur.system),
		IOWait: pct(prev.iowait, cur.iowait),
		Steal:  pct(prev.steal, cur.steal),
		Idle:   pct(prev.idle, cur.idle),
	}, true
}

// swapRates computes pages per second. A counter reset yields zeros:
// the baseline has already been reset by the caller, the next sample will be correct.
func swapRates(prev, cur *counters, elapsed float64) (in, out float64) {
	if !prev.swapKnown || !cur.swapKnown {
		return 0, 0
	}
	if cur.swapIn < prev.swapIn || cur.swapOut < prev.swapOut {
		return 0, 0
	}
	return round2(float64(cur.swapIn-prev.swapIn) / elapsed),
		round2(float64(cur.swapOut-prev.swapOut) / elapsed)
}

// readCPUTimes reads the aggregate cpu line from /proc/stat.
// nice is folded into user, irq and softirq into system: otherwise the sum of the
// components systematically falls short of 100. guest is not counted, it is already part of user.
func (c *Collector) readCPUTimes() (cpuTimes, error) {
	raw, err := c.readOnce("stat", "proc/stat", "failed to read /proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 8 {
			return cpuTimes{}, fmt.Errorf("the cpu line has %d fields, want at least 8", len(f))
		}
		v := make([]uint64, 8)
		for i := range v {
			n, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("failed to parse the cpu line: %w", err)
			}
			v[i] = n
		}
		t := cpuTimes{
			user:   v[0] + v[1],
			system: v[2] + v[5] + v[6],
			idle:   v[3],
			iowait: v[4],
			steal:  v[7],
		}
		t.total = t.user + t.system + t.idle + t.iowait + t.steal
		return t, nil
	}
	return cpuTimes{}, fmt.Errorf("the cpu line was not found")
}

// readSwapCounters reads pswpin/pswpout from /proc/vmstat: pages, not bytes.
func (c *Collector) readSwapCounters() (in, out uint64, err error) {
	raw, err := c.readOnce("vmstat", "proc/vmstat", "failed to read /proc/vmstat")
	if err != nil {
		return 0, 0, err
	}
	var gotIn, gotOut bool
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "pswpin":
			in, gotIn = n, true
		case "pswpout":
			out, gotOut = n, true
		}
	}
	if !gotIn || !gotOut {
		return 0, 0, fmt.Errorf("pswpin/pswpout were not found")
	}
	return in, out, nil
}

// cores returns the number of cores. With an unreadable /proc/cpuinfo it falls back to the runtime.
// The result is cached: there is no point re-reading /proc/cpuinfo on every tick.
func (c *Collector) cores() int {
	if c.coresOnce != 0 {
		return c.coresOnce
	}
	c.coresOnce = c.readCores()
	return c.coresOnce
}

func (c *Collector) readCores() int {
	raw, err := c.readOnce("cpuinfo", "proc/cpuinfo", "failed to read /proc/cpuinfo")
	if err != nil {
		return runtime.NumCPU()
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	if n == 0 {
		return runtime.NumCPU()
	}
	return n
}

func (c *Collector) uptime() uint64 {
	raw, err := c.readOnce("uptime", "proc/uptime", "failed to read /proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	return uint64(v)
}

func (c *Collector) loadavg() [3]float64 {
	var la [3]float64
	raw, err := c.readOnce("loadavg", "proc/loadavg", "failed to read /proc/loadavg")
	if err != nil {
		return la
	}
	f := strings.Fields(string(raw))
	for i := 0; i < 3 && i < len(f); i++ {
		la[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return la
}

// memory reads /proc/meminfo. The values there are in kilobytes, bytes go out.
func (c *Collector) memory() (Mem, Swap) {
	var m Mem
	var s Swap
	raw, err := c.readOnce("meminfo", "proc/meminfo", "failed to read /proc/meminfo")
	if err != nil {
		return m, s
	}
	var swapFree uint64
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(value)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		n *= 1024
		switch key {
		case "MemTotal":
			m.Total = n
		case "MemFree":
			m.Free = n
		case "MemAvailable":
			m.Available = n
		case "Buffers":
			m.Buffers = n
		case "Cached":
			m.Cached = n
		case "Shmem":
			m.Shmem = n
		case "SwapTotal":
			s.Total = n
		case "SwapFree":
			swapFree = n
		}
	}
	if s.Total > swapFree {
		s.Used = s.Total - swapFree
	}
	return m, s
}

// prettyName extracts PRETTY_NAME from /etc/os-release.
func prettyName(raw []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if ok && key == "PRETTY_NAME" {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

func (c *Collector) read(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.root, rel))
}

// warnOnce writes an error to the log once, not on every tick.
// The next error from the same source is logged again only after a success.
func (c *Collector) warnOnce(key, msg string, err error) {
	if c.logged[key] {
		return
	}
	c.logged[key] = true
	c.log.Warn(msg, "err", err)
}

// readOnce reads a file under the collector root, logging an error at most
// once in a row: repeating the same error on every tick clutters the log.
func (c *Collector) readOnce(key, rel, msg string) ([]byte, error) {
	raw, err := c.read(rel)
	if err != nil {
		c.warnOnce(key, msg, err)
		return nil, err
	}
	delete(c.logged, key)
	return raw, nil
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
