//go:build unix

package observability

import "golang.org/x/sys/unix"

// diskUsage returns the size of the filesystem holding path and the bytes
// available to unprivileged users on it.
func diskUsage(path string) (size, free uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := nonNegative(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, true
}

// nonNegative converts a block size, whose integer type differs per OS.
func nonNegative[T ~int32 | ~int64 | ~uint32 | ~uint64](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
