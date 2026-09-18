// Package input turns raw hardware reports from the Nano into stable, meaningful,
// typed application events.
//
// Everything here is pure arithmetic over integers and floats: no serial ports, no
// timers of its own, no logging side effects. The Nano smooths the ADC a little to
// avoid flooding the serial link; this package does the work that actually decides
// whether a human moved a knob on purpose.
//
// The processing chain for a potentiometer is:
//
//	raw ADC ─► Calibration ─► EMA Filter ─► (Deadband | HysteresisBands) ─► event
//	(0..4095)   (0.0..1.0)     (0.0..1.0)      volume %      channel index
package input

import (
	"fmt"
	"math"
)

// Calibration maps a raw ADC reading onto the normalised range 0.0..1.0.
//
// Real potentiometers rarely reach the rails: wiring resistance, ADC non-linearity
// near the supply rails (pronounced on the ESP32) and mechanical end stops mean a
// knob turned fully clockwise might read 4010 rather than 4095. Without end
// margins the user could never reach 100 % volume or the last channel.
type Calibration struct {
	// Min and Max are the raw readings that correspond to 0.0 and 1.0.
	Min, Max int
	// EndMarginPercent snaps the outer band of travel to the extremes. With the
	// default of 2, anything below 2 % reads as exactly 0.0 and anything above
	// 98 % reads as exactly 1.0.
	EndMarginPercent float64
	// Invert handles a pot wired with its outer legs swapped, so that clockwise
	// still increases the value without resoldering anything.
	Invert bool
}

// DefaultCalibration is the calibration for a 12-bit ESP32 ADC.
func DefaultCalibration() Calibration {
	return Calibration{Min: 0, Max: 4095, EndMarginPercent: 2}
}

// Validate reports whether the calibration can produce sensible output.
func (c Calibration) Validate() error {
	if c.Max <= c.Min {
		return fmt.Errorf("input: calibration max (%d) must exceed min (%d)", c.Max, c.Min)
	}
	if c.EndMarginPercent < 0 || c.EndMarginPercent >= 50 {
		return fmt.Errorf("input: end margin must be in [0,50), got %v", c.EndMarginPercent)
	}
	return nil
}

// Normalize converts a raw ADC reading to 0.0..1.0, applying end margins,
// inversion and clamping. Out-of-range readings are clamped rather than rejected:
// a spurious 5000 from a glitching ADC should read as "fully clockwise", not as an
// error that stalls the input pipeline.
func (c Calibration) Normalize(raw int) float64 {
	span := float64(c.Max - c.Min)
	if span <= 0 {
		return 0
	}
	v := (float64(raw) - float64(c.Min)) / span
	v = clamp01(v)

	if m := c.EndMarginPercent / 100; m > 0 {
		switch {
		case v <= m:
			v = 0
		case v >= 1-m:
			v = 1
		default:
			// Rescale the usable middle back across the full 0..1 range so the
			// knob's travel stays linear rather than having flat spots.
			v = (v - m) / (1 - 2*m)
		}
	}
	if c.Invert {
		v = 1 - v
	}
	return clamp01(v)
}

// Filter is an exponential moving average over normalised readings.
//
// An EMA is chosen over a sliding-window mean because it needs one float of state
// per pot, has no allocation, and its responsiveness is a single tunable. Alpha is
// the weight given to each new sample: 1.0 disables filtering, small values smooth
// harder at the cost of lag.
type Filter struct {
	alpha float64
	value float64
	// snapThreshold forces the filter to jump straight to a new reading when the
	// change is large. Without this, deliberately spinning a knob from one end to
	// the other would crawl towards the target over several samples, which feels
	// broken. Small changes (noise) are still smoothed.
	snapThreshold float64
	primed        bool
}

// NewFilter creates an EMA filter. Alpha is clamped to (0,1]; a snap threshold of
// 0 disables snapping.
func NewFilter(alpha, snapThreshold float64) *Filter {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}
	return &Filter{alpha: alpha, snapThreshold: snapThreshold}
}

// Update feeds a new normalised sample and returns the filtered value.
func (f *Filter) Update(v float64) float64 {
	v = clamp01(v)
	switch {
	case !f.primed:
		// The first reading is taken verbatim so that the knob's position at boot
		// is honoured immediately rather than ramped towards.
		f.value = v
		f.primed = true
	case f.snapThreshold > 0 && math.Abs(v-f.value) >= f.snapThreshold:
		f.value = v
	default:
		f.value += f.alpha * (v - f.value)
	}
	return f.value
}

// Value returns the current filtered value without feeding a sample.
func (f *Filter) Value() float64 { return f.value }

// Primed reports whether the filter has seen at least one sample. Until it has,
// the application has no idea where the knob physically is.
func (f *Filter) Primed() bool { return f.primed }

// Reset returns the filter to its unprimed state, which is what we want after the
// Nano reconnects: the knob may have been moved while the link was down.
func (f *Filter) Reset() {
	f.primed = false
	f.value = 0
}

// Deadband converts a continuous value into an integer percentage, emitting a
// change only when the new percentage differs from the last emitted one by at
// least Width. It is what stops a knob resting on a boundary from generating a
// stream of volume updates.
type Deadband struct {
	// Width is the minimum change in output units required to emit.
	Width int
	// Scale is the output range, e.g. 100 for a percentage.
	Scale int

	last    int
	emitted bool
}

// NewDeadband returns a percentage deadband of the given width.
func NewDeadband(width int) *Deadband {
	if width < 0 {
		width = 0
	}
	return &Deadband{Width: width, Scale: 100}
}

// Update maps v (0.0..1.0) to 0..Scale and reports the value plus whether it is a
// meaningful change worth acting on.
//
// The extremes are special-cased: reaching exactly 0 or exactly Scale always
// emits, because "I turned it all the way down" must silence the speaker even if
// the previous value was within the deadband.
func (d *Deadband) Update(v float64) (value int, changed bool) {
	scale := d.Scale
	if scale <= 0 {
		scale = 100
	}
	out := int(math.Round(clamp01(v) * float64(scale)))

	if !d.emitted {
		d.last, d.emitted = out, true
		return out, true
	}
	atExtreme := (out == 0 || out == scale) && out != d.last
	if atExtreme || abs(out-d.last) >= d.Width {
		d.last = out
		return out, true
	}
	return d.last, false
}

// Value returns the last emitted value.
func (d *Deadband) Value() int { return d.last }

// Emitted reports whether any value has been emitted yet.
func (d *Deadband) Emitted() bool { return d.emitted }

// Reset clears the deadband state.
func (d *Deadband) Reset() { d.emitted, d.last = false, 0 }

// HysteresisBands divides 0.0..1.0 into N equal regions and reports which region a
// value falls in, requiring the value to travel past the boundary by a margin
// before the reported band changes.
//
// This is what makes an absolute-position potentiometer usable as a channel
// selector: with N channels the knob's travel is split into N stable regions, and
// ADC noise sitting exactly on a boundary cannot flip the channel back and forth.
//
//	band:      0        1        2        3
//	         |------|--------|--------|------|
//	                ^        ^        ^
//	             boundaries, each widened by ±Margin
type HysteresisBands struct {
	// Count is the number of bands. Zero means "no bands available", in which case
	// Update always reports -1.
	Count int
	// Margin is the extra normalised distance the value must travel past a
	// boundary to change bands. It is expressed as a fraction of one band's width,
	// so it stays sensible as the channel count changes: 0.25 means a quarter of a
	// band.
	Margin float64

	current int
	primed  bool
}

// NewHysteresisBands returns a band mapper. A margin is clamped to [0, 0.49) so
// that opposing boundaries can never overlap.
func NewHysteresisBands(count int, margin float64) *HysteresisBands {
	if margin < 0 {
		margin = 0
	}
	if margin > 0.49 {
		margin = 0.49
	}
	return &HysteresisBands{Count: count, Margin: margin, current: -1}
}

// SetCount changes the number of bands, which happens whenever the ErsatzTV
// channel list changes. The current band is re-derived from the last value so that
// the knob's physical position still selects sensibly after a channel is added.
func (h *HysteresisBands) SetCount(count, fromValue int) {
	_ = fromValue
	if count < 0 {
		count = 0
	}
	h.Count = count
	h.primed = false
	h.current = -1
}

// Band returns the currently selected band, or -1 when nothing is selected.
func (h *HysteresisBands) Band() int { return h.current }

// Update reports the band for v and whether the selection changed.
func (h *HysteresisBands) Update(v float64) (band int, changed bool) {
	if h.Count <= 0 {
		was := h.current
		h.current, h.primed = -1, false
		return -1, was != -1
	}
	v = clamp01(v)
	raw := RawBand(v, h.Count)

	if !h.primed {
		h.current, h.primed = raw, true
		return h.current, true
	}
	if raw == h.current {
		return h.current, false
	}

	// Require the value to be past the shared boundary by Margin band-widths
	// before accepting the move. Moving more than one band at a time (a deliberate
	// sweep) is always accepted immediately.
	width := 1.0 / float64(h.Count)
	if abs(raw-h.current) > 1 {
		h.current = raw
		return h.current, true
	}
	var boundary float64
	if raw > h.current {
		boundary = float64(h.current+1) * width
		if v < boundary+h.Margin*width {
			return h.current, false
		}
	} else {
		boundary = float64(h.current) * width
		if v > boundary-h.Margin*width {
			return h.current, false
		}
	}
	h.current = raw
	return h.current, true
}

// Reset forgets the current selection.
func (h *HysteresisBands) Reset() { h.current, h.primed = -1, false }

// RawBand returns the band index for v with no hysteresis applied. Exported
// because both the band mapper and the API's "where is the knob" reporting want it.
func RawBand(v float64, count int) int {
	if count <= 0 {
		return -1
	}
	b := int(clamp01(v) * float64(count))
	if b >= count { // v == 1.0 lands one past the end
		b = count - 1
	}
	if b < 0 {
		b = 0
	}
	return b
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
