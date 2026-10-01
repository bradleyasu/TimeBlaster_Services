package system

import "testing"

func TestDiskStatUsedPercent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total, free uint64
		want        float64
	}{
		{"half full", 1000, 500, 50},
		{"empty filesystem", 1000, 1000, 0},
		{"completely full", 1000, 0, 100},
		// The device's real shape: a large disk nearly full of media.
		{"nearly full large disk", 469_000_000_000, 20_000_000_000, 95.736},
		// An unknown size must not report a confident 100%.
		{"unknown size", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DiskStat{TotalBytes: tc.total, AvailBytes: tc.free}.UsedPercent()
			if diff := got - tc.want; diff > 0.01 || diff < -0.01 {
				t.Errorf("UsedPercent() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStatfsReadsARealFilesystem(t *testing.T) {
	// A sanity check against the kernel rather than a fake: the byte-size
	// arithmetic is where a statfs wrapper usually goes wrong, and a block-count
	// mistake shows up immediately as an absurd total.
	st, err := Statfs{}.Stat(t.TempDir())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.TotalBytes == 0 {
		t.Fatal("reported a filesystem with no size")
	}
	if st.AvailBytes > st.TotalBytes {
		t.Errorf("more space available (%d) than exists (%d)", st.AvailBytes, st.TotalBytes)
	}
	// Any real filesystem backing a temp dir is at least a megabyte.
	if st.TotalBytes < 1<<20 {
		t.Errorf("implausible total size %d bytes; block arithmetic is probably wrong", st.TotalBytes)
	}
	if p := st.UsedPercent(); p < 0 || p > 100 {
		t.Errorf("used percent out of range: %v", p)
	}
}

func TestStatfsFailsOnAMissingPath(t *testing.T) {
	if _, err := (Statfs{}).Stat("/definitely/not/a/path/here"); err == nil {
		t.Fatal("statfs on a missing path should fail rather than report zero space")
	}
}
