//go:build linux

package metrics

import "syscall"

// statfs reads the usage of a mount point. Total and Used come from Blocks/Bfree —
// that is the physical disk volume. Avail comes from Bavail, which is smaller by the
// blocks reserved for root: the server computes the used percentage from Used/(Used+Avail),
// the same way df does.
func statfs(path string) (usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return usage{}, err
	}
	bs := uint64(st.Bsize)
	u := usage{
		Total:       st.Blocks * bs,
		InodesTotal: st.Files,
	}
	if st.Blocks >= st.Bfree {
		u.Used = (st.Blocks - st.Bfree) * bs
	}
	u.Avail = st.Bavail * bs
	if st.Files >= st.Ffree {
		u.InodesUsed = st.Files - st.Ffree
	}
	return u, nil
}
