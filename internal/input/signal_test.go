package input

import (
	"math"
	"testing"
)

func TestCalibrationNormalize(t *testing.T) {
	c := Calibration{Min: 0, Max: 4095, EndMarginPercent: 0}
	tests := []struct {
		raw  int
		want float64
	}{
		{0, 0}, {4095, 1}, {2047, 0.4999}, {-500, 0}, {9999, 1},
	}
	for _, tc := range tests {
		if got := c.Normalize(tc.raw); math.Abs(got-tc.want) > 0.001 {
			t.Errorf("Normalize(%d) = %v want ~%v", tc.raw, got, tc.want)
		}
	}
}

func TestCalibrationEndMarginsReachTheExtremes(t *testing.T) {
	// A pot that physically tops out short of the rail must still reach 1.0. With a
	// 2 % margin the top 82 counts of a 12-bit ADC all read as fully clockwise.
	c := Calibration{Min: 0, Max: 4095, EndMarginPercent: 2}
	if got := c.Normalize(4020); got != 1 {
		t.Errorf("near-max reading should snap to 1.0, got %v", got)
	}
	if got := c.Normalize(40); got != 0 {
		t.Errorf("near-min reading should snap to 0.0, got %v", got)
	}
	// A pot with a wider dead zone needs a wider margin; check the knob is tunable.
	wide := Calibration{Min: 0, Max: 4095, EndMarginPercent: 5}
	if got := wide.Normalize(3950); got != 1 {
		t.Errorf("wide margin should snap 3950 to 1.0, got %v", got)
	}
	// The usable middle is rescaled so travel stays linear (no flat spot).
	mid := c.Normalize(2047)
	if math.Abs(mid-0.5) > 0.01 {
		t.Errorf("midpoint should stay near 0.5, got %v", mid)
	}
}

func TestCalibrationInvert(t *testing.T) {
	c := Calibration{Min: 0, Max: 4095, Invert: true}
	if got := c.Normalize(0); got != 1 {
		t.Errorf("inverted min: %v", got)
	}
	if got := c.Normalize(4095); got != 0 {
		t.Errorf("inverted max: %v", got)
	}
}

func TestCalibrationValidate(t *testing.T) {
	if err := DefaultCalibration().Validate(); err != nil {
		t.Fatalf("default calibration invalid: %v", err)
	}
	for _, bad := range []Calibration{
		{Min: 100, Max: 100},
		{Min: 0, Max: 4095, EndMarginPercent: -1},
		{Min: 0, Max: 4095, EndMarginPercent: 50},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v should be invalid", bad)
		}
	}
}

func TestCalibrationDegenerateRangeDoesNotPanic(t *testing.T) {
	c := Calibration{Min: 10, Max: 10}
	if got := c.Normalize(10); got != 0 {
		t.Errorf("got %v", got)
	}
}

func TestFilterTakesFirstSampleVerbatim(t *testing.T) {
	f := NewFilter(0.3, 0)
	if f.Primed() {
		t.Fatal("new filter should not be primed")
	}
	if got := f.Update(0.8); got != 0.8 {
		t.Errorf("first sample should pass through, got %v", got)
	}
	if !f.Primed() {
		t.Error("filter should be primed after one sample")
	}
}

func TestFilterSmoothsNoise(t *testing.T) {
	f := NewFilter(0.2, 0) // snapping disabled so smoothing is observable
	f.Update(0.5)
	// A single-sample spike must not move the output far.
	got := f.Update(0.9)
	if got <= 0.5 || got >= 0.62 {
		t.Errorf("spike moved the filter too far or not at all: %v", got)
	}
	// Repeated samples converge on the new value.
	for range 40 {
		got = f.Update(0.9)
	}
	if math.Abs(got-0.9) > 0.01 {
		t.Errorf("filter did not converge: %v", got)
	}
}

func TestFilterSnapsOnLargeDeliberateMoves(t *testing.T) {
	f := NewFilter(0.2, 0.1)
	f.Update(0.1)
	if got := f.Update(0.9); got != 0.9 {
		t.Errorf("a large move should snap immediately, got %v", got)
	}
	// Small moves are still smoothed.
	if got := f.Update(0.93); got == 0.93 {
		t.Errorf("a small move should be smoothed, got %v", got)
	}
}

func TestFilterReset(t *testing.T) {
	f := NewFilter(0.3, 0)
	f.Update(0.7)
	f.Reset()
	if f.Primed() || f.Value() != 0 {
		t.Fatalf("reset left state behind: primed=%v value=%v", f.Primed(), f.Value())
	}
	if got := f.Update(0.2); got != 0.2 {
		t.Errorf("after reset the next sample should pass through, got %v", got)
	}
}

func TestFilterClampsAlpha(t *testing.T) {
	for _, alpha := range []float64{0, -1, 5} {
		f := NewFilter(alpha, 0)
		f.Update(0)
		if got := f.Update(1); got <= 0 || got > 1 {
			t.Errorf("alpha %v produced %v", alpha, got)
		}
	}
}

func TestDeadbandSuppressesJitter(t *testing.T) {
	d := NewDeadband(3)

	if v, changed := d.Update(0.50); !changed || v != 50 {
		t.Fatalf("first update: %d, %v", v, changed)
	}
	// 1 % of jitter is below the deadband and must be swallowed.
	for _, v := range []float64{0.51, 0.49, 0.515, 0.492} {
		if got, changed := d.Update(v); changed {
			t.Errorf("jitter at %v emitted %d", v, got)
		}
	}
	// A deliberate move clears the deadband.
	if v, changed := d.Update(0.56); !changed || v != 56 {
		t.Errorf("deliberate move: %d, %v", v, changed)
	}
}

func TestDeadbandAlwaysEmitsAtTheExtremes(t *testing.T) {
	// Turning the knob fully down must silence the speaker even though the change
	// from 2 % to 0 % is inside a 5-point deadband.
	d := NewDeadband(5)
	d.Update(0.02)
	if v, changed := d.Update(0.0); !changed || v != 0 {
		t.Errorf("zero: %d, %v", v, changed)
	}
	d.Update(0.98)
	if v, changed := d.Update(1.0); !changed || v != 100 {
		t.Errorf("full: %d, %v", v, changed)
	}
	// Staying at the extreme does not re-emit.
	if _, changed := d.Update(1.0); changed {
		t.Error("staying at full scale re-emitted")
	}
}

func TestDeadbandReset(t *testing.T) {
	d := NewDeadband(3)
	d.Update(0.5)
	d.Reset()
	if d.Emitted() {
		t.Fatal("reset did not clear emitted")
	}
	if v, changed := d.Update(0.5); !changed || v != 50 {
		t.Errorf("after reset: %d, %v", v, changed)
	}
}

func TestRawBand(t *testing.T) {
	tests := []struct {
		v     float64
		count int
		want  int
	}{
		{0.0, 10, 0}, {0.05, 10, 0}, {0.15, 10, 1}, {0.99, 10, 9}, {1.0, 10, 9},
		{0.5, 1, 0}, {0.5, 0, -1}, {-0.5, 4, 0}, {1.5, 4, 3},
	}
	for _, tc := range tests {
		if got := RawBand(tc.v, tc.count); got != tc.want {
			t.Errorf("RawBand(%v, %d) = %d want %d", tc.v, tc.count, got, tc.want)
		}
	}
}

func TestHysteresisBandsRequireDeliberateMovement(t *testing.T) {
	// 10 channels => each band is 10 % wide; margin 0.25 => 2.5 % of extra travel.
	h := NewHysteresisBands(10, 0.25)

	if band, changed := h.Update(0.05); band != 0 || !changed {
		t.Fatalf("initial: %d, %v", band, changed)
	}
	// Sitting right on the 0/1 boundary with noise must not flip channels.
	for _, v := range []float64{0.099, 0.101, 0.098, 0.102, 0.1005} {
		if band, changed := h.Update(v); changed {
			t.Errorf("noise at %v switched to band %d", v, band)
		}
	}
	// Moving properly into band 1 (past 0.10 + 0.025) is accepted.
	if band, changed := h.Update(0.13); band != 1 || !changed {
		t.Errorf("deliberate move: %d, %v", band, changed)
	}
	// Coming back must also clear the margin on the other side.
	if band, changed := h.Update(0.099); changed {
		t.Errorf("barely re-crossing switched to %d", band)
	}
	if band, changed := h.Update(0.06); band != 0 || !changed {
		t.Errorf("returning: %d, %v", band, changed)
	}
}

func TestHysteresisBandsAcceptFastSweeps(t *testing.T) {
	// Spinning the knob across several bands should land where the user put it,
	// not crawl one band at a time.
	h := NewHysteresisBands(10, 0.25)
	h.Update(0.05)
	if band, changed := h.Update(0.85); band != 8 || !changed {
		t.Errorf("sweep: %d, %v", band, changed)
	}
}

func TestHysteresisBandsWithNoChannels(t *testing.T) {
	h := NewHysteresisBands(0, 0.25)
	band, changed := h.Update(0.5)
	if band != -1 {
		t.Errorf("band with no channels: %d", band)
	}
	_ = changed
	if band, _ := h.Update(0.9); band != -1 {
		t.Errorf("still no channels: %d", band)
	}
}

func TestHysteresisBandsSetCountReselects(t *testing.T) {
	h := NewHysteresisBands(4, 0.25)
	h.Update(0.9) // band 3 of 4
	if h.Band() != 3 {
		t.Fatalf("band: %d", h.Band())
	}
	// Channel list shrinks to 2; the same knob position now means band 1.
	h.SetCount(2, 0)
	band, changed := h.Update(0.9)
	if band != 1 || !changed {
		t.Errorf("after SetCount: %d, %v", band, changed)
	}
}

func TestHysteresisBandsClampMargin(t *testing.T) {
	if h := NewHysteresisBands(4, 5); h.Margin > 0.49 {
		t.Errorf("margin not clamped: %v", h.Margin)
	}
	if h := NewHysteresisBands(4, -1); h.Margin != 0 {
		t.Errorf("negative margin not clamped: %v", h.Margin)
	}
}

func TestHysteresisBandsSingleChannel(t *testing.T) {
	h := NewHysteresisBands(1, 0.25)
	for _, v := range []float64{0, 0.5, 1} {
		if band, _ := h.Update(v); band != 0 {
			t.Errorf("single channel at %v gave %d", v, band)
		}
	}
}

// The end-to-end signal chain: a noisy ADC resting near a channel boundary must
// produce exactly one channel change, not a stream of them.
func TestSignalChainIsStableUnderNoise(t *testing.T) {
	cal := DefaultCalibration()
	f := NewFilter(0.35, 0.08)
	h := NewHysteresisBands(10, 0.25)

	changes := 0
	// Raw ~410 is the 0/1 boundary for 10 channels on a 12-bit ADC.
	noise := []int{405, 412, 408, 415, 402, 411, 409, 414, 406, 410, 413, 407}
	for _, raw := range noise {
		if _, changed := h.Update(f.Update(cal.Normalize(raw))); changed {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("noise around a boundary produced %d channel changes, want 1 (the initial selection)", changes)
	}
}
