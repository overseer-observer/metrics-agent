package metrics

import (
	"context"
	"sort"
	"strings"
	"time"
)

// usage is what statfs returns for a mount point.
type usage struct {
	Total       uint64
	Used        uint64
	Avail       uint64
	InodesTotal uint64
	InodesUsed  uint64
}

type statfsFunc func(path string) (usage, error)

// pseudoFSTypes are filesystems without a real device: computing used space
// for them is meaningless.
var pseudoFSTypes = map[string]bool{
	"autofs": true, "bpf": true, "binfmt_misc": true, "cgroup": true,
	"configfs": true, "debugfs": true, "devpts": true, "devtmpfs": true,
	"efivarfs": true, "fusectl": true, "hugetlbfs": true,
	"mqueue": true, "nsfs": true, "overlay": true, "proc": true, "pstore": true,
	"ramfs": true, "rpc_pipefs": true, "securityfs": true, "selinuxfs": true,
	"squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true,
}

// hangProneFSPrefixes are filesystems whose statfs may never return:
// network ones with an unreachable server and any fuse.* with a dead backend process
// (sshfs, s3fs, gvfsd). The goroutine from statfsWithTimeout hangs forever on such a
// call, and a sample is taken every tick, so such mount points are not touched at all.
// The trailing dot in the prefix is deliberate: fuseblk is a local disk via ntfs-3g.
var hangProneFSPrefixes = []string{"nfs", "cifs", "smb", "ceph", "glusterfs", "afs", "9p", "fuse."}

// mount is a line from /proc/mounts, with the fields we need.
type mount struct {
	device string
	point  string
	fstype string
}

// filesystems returns the filesystems of a sample: real ones, without duplicates,
// at most the maxFilesystems largest.
func (c *Collector) filesystems(ctx context.Context) []Filesystem {
	raw, err := c.readOnce("mounts", "proc/mounts", "failed to read /proc/mounts")
	if err != nil {
		return nil
	}

	out := make([]Filesystem, 0, 16)
	for _, m := range parseMounts(string(raw)) {
		key := "statfs:" + m.point
		u, err := c.statfsWithTimeout(ctx, m.point)
		if err != nil {
			// A stuck mount point fails on every tick: log it only once in a row.
			c.warnOnce(key, "statfs did not complete, mount point skipped: "+m.point, err)
			continue
		}
		delete(c.logged, key)
		out = append(out, Filesystem{
			Mount:       m.point,
			FSType:      m.fstype,
			Total:       u.Total,
			Used:        u.Used,
			Avail:       u.Avail,
			InodesTotal: u.InodesTotal,
			InodesUsed:  u.InodesUsed,
		})
	}

	if len(out) > c.maxFilesystems {
		c.log.Warn("more filesystems than the limit, keeping the largest",
			"found", len(out), "limit", c.maxFilesystems)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
		out = out[:c.maxFilesystems]
	}
	return out
}

// parseMounts parses /proc/mounts, discarding pseudo and hang-prone filesystems
// and repeats of the same device: that is what bind mounts look like, and counting
// the same disk several times is meaningless. The first mount point is kept.
func parseMounts(raw string) []mount {
	seen := make(map[string]bool)
	var out []mount
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		m := mount{device: unescape(f[0]), point: unescape(f[1]), fstype: f[2]}
		if skipFSType(m.fstype) {
			continue
		}
		if seen[m.device] {
			continue
		}
		seen[m.device] = true
		out = append(out, m)
	}
	return out
}

func skipFSType(fstype string) bool {
	if pseudoFSTypes[fstype] || strings.HasPrefix(fstype, "cgroup") {
		return true
	}
	for _, p := range hangProneFSPrefixes {
		if strings.HasPrefix(fstype, p) {
			return true
		}
	}
	return false
}

// unescape expands the octal sequences the kernel uses to encode
// spaces and tabs in /proc/mounts.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v byte
			ok := true
			for _, d := range []byte(s[i+1 : i+4]) {
				if d < '0' || d > '7' {
					ok = false
					break
				}
				v = v*8 + (d - '0')
			}
			if ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// statfsWithTimeout calls statfs in a separate goroutine: on a hung mount point
// the call itself is not interrupted, but the collection does not wait for it.
func (c *Collector) statfsWithTimeout(ctx context.Context, path string) (usage, error) {
	type result struct {
		u   usage
		err error
	}
	// A buffer of one: the goroutine must not get stuck on the send after a timeout.
	ch := make(chan result, 1)
	go func() {
		u, err := c.statfs(path)
		ch <- result{u, err}
	}()

	timer := time.NewTimer(c.statfsTimeout)
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.u, r.err
	case <-timer.C:
		return usage{}, errStatfsTimeout
	case <-ctx.Done():
		return usage{}, ctx.Err()
	}
}
