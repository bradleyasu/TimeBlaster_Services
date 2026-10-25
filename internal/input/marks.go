package input

import "math"

// bandMapper turns a filtered knob position into a channel band. The channel
// knob uses HysteresisBands -- equal slices, one per channel -- unless it has
// been calibrated to printed marks, when it uses MarkedBands.
type bandMapper interface {
	SetCount(count, fromValue int)
	Update(v float64) (band int, changed bool)
	Reset()
	Len() int
}

// Len returns the number of bands.
func (h *HysteresisBands) Len() int { return h.Count }

// MarkedBands maps a knob to positions measured from printed marks on a dial.
//
// Equal slices only line up with printed marks when the marks are evenly
// spaced, there are exactly as many as channels, the pot reads linearly, and
// nothing limits its travel. A dial in a housing meets none of those reliably,
// so instead each mark's actual reading is measured, and the knob selects the
// nearest mark. Each mark owns the travel halfway to its neighbours, which also
// means anything beyond the first or last mark stays on it.
//
// Mark k is channel k, whatever the channel count: a mark with no channel
// behind it reports -1, the standby screen.
type MarkedBands struct {
	// marks are the normalised readings at each mark, first mark first. They
	// may descend, for a pot that reads backwards.
	marks []float64
	// Margin is how far past the midpoint between two marks the knob must
	// travel to change, as a fraction of the distance between them -- the
	// same meaning as HysteresisBands.Margin.
	Margin float64

	count   int
	current int // the selected mark, or -1
	band    int // what was last reported
	primed  bool
}

// NewMarkedBands returns a mapper over marks, which must be strictly monotonic.
func NewMarkedBands(marks []float64, margin float64) *MarkedBands {
	if margin < 0 {
		margin = 0
	}
	if margin > 0.49 {
		margin = 0.49
	}
	return &MarkedBands{marks: append([]float64(nil), marks...), Margin: margin, current: -1, band: -1}
}

// SetCount sets how many channels there are. Marks beyond the count have no
// channel behind them.
func (m *MarkedBands) SetCount(count, _ int) {
	if count < 0 {
		count = 0
	}
	m.count = count
	m.Reset()
}

// Len returns how many channels the knob can reach: the channel count, or the
// number of marks if there are fewer marks than channels.
func (m *MarkedBands) Len() int { return min(m.count, len(m.marks)) }

// Reset forgets the current selection.
func (m *MarkedBands) Reset() { m.current, m.band, m.primed = -1, -1, false }

// Update reports the band for v and whether it changed.
func (m *MarkedBands) Update(v float64) (int, bool) {
	if m.count <= 0 || len(m.marks) == 0 {
		was := m.band
		m.current, m.band, m.primed = -1, -1, false
		return -1, was != -1
	}
	v = clamp01(v)
	nearest := m.nearest(v)
	// The first update after a reset always counts as a change, even if it
	// reports -1: it is the knob's first word on the matter.
	first := !m.primed

	switch {
	case first:
		m.current, m.primed = nearest, true
	case nearest == m.current:
	case abs(nearest-m.current) > 1:
		// A deliberate sweep past a whole mark is accepted at once.
		m.current = nearest
	default:
		// The next mark along: only once the knob is Margin past the midpoint,
		// so a dial resting between two marks does not flicker.
		from, to := m.marks[m.current], m.marks[nearest]
		mid := (from + to) / 2
		dir := math.Copysign(1, to-from)
		if (v-mid)*dir >= m.Margin*math.Abs(to-from) {
			m.current = nearest
		}
	}

	band := m.current
	if band >= m.count {
		band = -1
	}
	changed := first || band != m.band
	m.band = band
	return band, changed
}

func (m *MarkedBands) nearest(v float64) int {
	best, bestDist := 0, math.Inf(1)
	for i, mark := range m.marks {
		if d := math.Abs(v - mark); d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}
