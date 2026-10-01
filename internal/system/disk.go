package system

// DiskStat describes space on one filesystem.
type DiskStat struct {
	TotalBytes uint64
	// AvailBytes is the space an unprivileged process may still use. It is
	// deliberately not the same as total free space: filesystems reserve a
	// fraction for root, and a daemon that is not root cannot have it. Reporting
	// the larger number would promise room the device does not have.
	AvailBytes uint64
}

// UsedPercent is how full the filesystem is, 0 when its size is unknown.
func (d DiskStat) UsedPercent() float64 {
	if d.TotalBytes == 0 {
		return 0
	}
	used := d.TotalBytes - d.AvailBytes
	return float64(used) / float64(d.TotalBytes) * 100
}

// Disk reports free space for a path.
type Disk interface {
	Stat(path string) (DiskStat, error)
}

// FakeDisk is a Disk whose answers a test chooses.
type FakeDisk struct {
	Stat_ DiskStat
	Err   error
	// Paths records what was asked about, so a test can assert the right
	// filesystem is being watched.
	Paths []string
}

func (f *FakeDisk) Stat(path string) (DiskStat, error) {
	f.Paths = append(f.Paths, path)
	if f.Err != nil {
		return DiskStat{}, f.Err
	}
	return f.Stat_, nil
}
