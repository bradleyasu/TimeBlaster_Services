//go:build !linux && !darwin

package system

import "errors"

// Statfs is unavailable on this platform. The device is a Raspberry Pi; this
// exists so the package still builds elsewhere.
type Statfs struct{}

func (Statfs) Stat(string) (DiskStat, error) {
	return DiskStat{}, errors.New("system: free space is not available on this platform")
}
