package metrics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMountsFiltersAndDeduplicates(t *testing.T) {
	// /var/lib/docker in the dump is a bind mount of the same /dev/sda1 as the root.
	raw, err := os.ReadFile("testdata/base/proc/mounts")
	if err != nil {
		t.Fatal(err)
	}
	got := parseMounts(string(raw))

	want := []mount{
		{device: "/dev/sda1", point: "/", fstype: "ext4"},
		{device: "/dev/sdb1", point: "/data", fstype: "xfs"},
		{device: "/dev/sdc1", point: "/mnt/with space", fstype: "ext4"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mount points: %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount point %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// writeMounts places a prepared /proc/mounts into a temporary root.
func writeMounts(t *testing.T, lines string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/mounts"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFilesystemsKeepsLargestWithinLimit(t *testing.T) {
	const total = 40
	var b strings.Builder
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "/dev/vd%d /mnt/d%02d ext4 rw 0 0\n", i, i)
	}
	c, buf := newTestCollector(t, writeMounts(t, b.String()))
	// The volume grows with the number: the largest ones are the mount points with higher indices.
	c.statfs = func(path string) (usage, error) {
		var n int
		fmt.Sscanf(filepath.Base(path), "d%d", &n)
		return usage{Total: uint64(n+1) * 1024, Used: 512}, nil
	}

	got := c.filesystems(context.Background())
	if len(got) != DefaultMaxFilesystems {
		t.Fatalf("%d filesystems made it into the sample, want %d", len(got), DefaultMaxFilesystems)
	}
	for _, fs := range got {
		if fs.Total < uint64(total-DefaultMaxFilesystems+1)*1024 {
			t.Errorf("a small filesystem %s (%d bytes) remained in the sample", fs.Mount, fs.Total)
		}
	}
	if !strings.Contains(buf.String(), "more filesystems than the limit") {
		t.Error("exceeding the limit was not logged")
	}
}

func TestFilesystemsStatfsTimeoutDoesNotBlock(t *testing.T) {
	root := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n/dev/sdb1 /hang ext4 rw 0 0\n")
	c, buf := newTestCollector(t, root)
	c.statfsTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c.statfs = func(path string) (usage, error) {
		if path == "/hang" {
			<-release
			return usage{}, nil
		}
		return usage{Total: 1024}, nil
	}

	start := time.Now()
	got := c.filesystems(context.Background())
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("the collection waited %v for a hung statfs", elapsed)
	}
	if len(got) != 1 || got[0].Mount != "/" {
		t.Fatalf("only the live filesystem must make it into the sample, got %+v", got)
	}
	if !strings.Contains(buf.String(), "statfs did not complete") {
		t.Error("the statfs timeout was not logged")
	}
}

func TestUnescape(t *testing.T) {
	if got := unescape(`/mnt/with\040space`); got != "/mnt/with space" {
		t.Errorf("unescape = %q", got)
	}
	if got := unescape("/mnt/plain"); got != "/mnt/plain" {
		t.Errorf("unescape = %q", got)
	}
}

func TestParseMountsSkipsHangProneFUSE(t *testing.T) {
	got := parseMounts(strings.Join([]string{
		"/dev/sda1 / ext4 rw 0 0",
		"sshfs#user@host:/ /mnt/remote fuse.sshfs rw 0 0",
		"s3fs /mnt/bucket fuse.s3fs rw 0 0",
		"/dev/sdb1 /mnt/win fuseblk rw 0 0",
	}, "\n"))

	want := []mount{
		{device: "/dev/sda1", point: "/", fstype: "ext4"},
		{device: "/dev/sdb1", point: "/mnt/win", fstype: "fuseblk"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mount points: %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount point %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
