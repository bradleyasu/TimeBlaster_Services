//go:build linux || darwin

package system

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Statfs reads free space from the kernel.
type Statfs struct{}

func (Statfs) Stat(path string) (DiskStat, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskStat{}, fmt.Errorf("system: statfs %s: %w", path, err)
	}
	// Bsize is int64 on Linux and uint32 on Darwin, so it is widened rather than
	// used directly.
	bsize := uint64(st.Bsize)
	return DiskStat{
		TotalBytes: st.Blocks * bsize,
		AvailBytes: st.Bavail * bsize,
	}, nil
}
