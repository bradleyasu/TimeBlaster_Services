package guide

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bradsheets/timeblaster/internal/ersatztv"
	"github.com/bradsheets/timeblaster/internal/mpv"
)

// The guide is drawn on mpv's overlay canvas, 1280x720 whatever the
// television's real resolution. The picture is shrunk into the top-left
// quarter; the guide paints everything else.
const (
	canvasW = mpv.OverlayResX
	canvasH = mpv.OverlayResY

	videoW = canvasW / 2
	videoH = canvasH / 2

	// The grid fills the bottom half: a header of time slots, then rows.
	gridY       = videoH
	headerH     = 40
	rowH        = 64
	RowsPerPage = (canvasH - gridY - headerH) / rowH

	chanW = 240
	timeX = chanW
	timeW = canvasW - chanW
	// Window is how much schedule the grid spans: three half-hour columns.
	Window = 90 * time.Minute
	slot   = 30 * time.Minute

	gap = 3

	font = "DejaVu Sans Mono"
	// charWidth is DejaVu Sans Mono's advance as a fraction of the ASS font
	// size, measured from a screenshot of the real television and rounded up
	// so truncation errs towards fitting.
	charWidth = 0.53
)

// Colours, as #RRGGBB: the Timeblaster's purples, with Nickelodeon's slime
// green (Pantone 375) where the old guide channel used yellow.
const (
	colBackground = "#2A0E4A"
	colHeader     = "#1A0833"
	colChannel    = "#3B1667"
	colProgramme  = "#4C1D8A"
	colHighlight  = "#6B2FC7"
	colAccent     = "#97D700"
	colText       = "#FFFFFF"
	colSubtle     = "#D8C8F5"
	colBrand      = colAccent
)

// Frame is everything one drawing of the guide depends on.
type Frame struct {
	Now      time.Time
	Clock24h bool
	Channels []ersatztv.GuideChannel
	// Playing is the number of the channel in the corner, or "".
	Playing string
	// Page is which page of rows to show; it wraps.
	Page int
	// Tuning covers the corner while a new channel comes up. mpv holds the
	// old channel's last frame until the new one decodes, which under the new
	// channel's listing reads as the wrong programme.
	Tuning bool
	// WiFi is whether the Timeblaster is on the home Wi-Fi, shown as a symbol
	// beside the clock.
	WiFi bool
}

// Pages reports how many pages of rows the channels need.
func Pages(channels int) int {
	if channels <= 0 {
		return 1
	}
	return (channels + RowsPerPage - 1) / RowsPerPage
}

// PageOf reports the page a channel's row is on, or -1.
func PageOf(channels []ersatztv.GuideChannel, number string) int {
	for i, c := range channels {
		if c.Number == number {
			return i / RowsPerPage
		}
	}
	return -1
}

// Render draws the guide as ASS events for mpv's osd-overlay. It is a pure
// function of the frame, so the layout can be tested without a television --
// and so the caller can skip redrawing when nothing visible has changed.
func Render(f Frame) string {
	var b strings.Builder

	// Background everywhere but the picture: the right of the top half, and
	// the whole bottom half.
	rect(&b, videoW, 0, canvasW-videoW, videoH, colBackground)
	rect(&b, 0, gridY, canvasW, canvasH-gridY, colBackground)

	renderPanel(&b, f)
	renderGrid(&b, f)
	if f.Tuning {
		rect(&b, 0, 0, videoW, videoH, colBackground)
		text(&b, videoW/2, videoH/2-24, 5, 56, colText, true, "CH "+f.Playing)
		text(&b, videoW/2, videoH/2+36, 5, 28, colAccent, true, "TUNING…")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// The top-right panel, laid out from the top: the name and the time on one
// line, the date, then what is on the corner channel -- its title, the
// episode, a synopsis of up to descLines lines, and when it airs.
const (
	panelX     = videoW + 36
	panelRight = canvasW - 30
	panelW     = panelRight - panelX
	descSize   = 19
	descLineH  = 23
	descLines  = 5
)

// renderPanel draws the top right: the name, the time, and what is on the
// channel in the corner.
func renderPanel(b *strings.Builder, f Frame) {
	x := panelX
	text(b, x, 20, 7, 28, colBrand, true, "TIMEBLASTER GUIDE")
	clock := timeText(f.Now, f.Clock24h)
	text(b, panelRight, 16, 9, 34, colText, true, clock)
	// The Wi-Fi symbol sits just left of the clock, centred on the digits and
	// about as tall as them. The symbol's point is at the bottom of its box,
	// so its centre is half its height above the point.
	clockW := int(float64(utf8.RuneCountInString(clock)) * 34 * charWidth)
	wifiIcon(b, panelRight-clockW-20-wifiIconR, clockMidY+(wifiIconR+1)/2, f.WiFi)
	text(b, x, 60, 7, 20, colAccent, false, strings.ToUpper(f.Now.Format("Monday, Jan 2")))

	ch := find(f.Channels, f.Playing)
	if ch == nil {
		text(b, x, 132, 7, 24, colSubtle, false, "TURN THE CHANNEL KNOB")
		return
	}
	text(b, x, 98, 7, 22, colAccent, true,
		truncate(fmt.Sprintf("NOW ON %s  %s", ch.Number, strings.ToUpper(ch.Name)), chars(panelW, 22)))

	p := current(ch.Programmes, f.Now)
	if p == nil {
		text(b, x, 132, 7, 28, colSubtle, false, "NO LISTING")
		return
	}
	text(b, x, 130, 7, 28, colText, true, truncate(p.Title, chars(panelW, 28)))
	y := 168
	if p.SubTitle != "" {
		text(b, x, y, 7, 22, colSubtle, false, truncate("\""+p.SubTitle+"\"", chars(panelW, 22)))
		y += 32
	}
	for i, line := range wrap(p.Description, chars(panelW, descSize), descLines) {
		text(b, x, y+i*descLineH, 7, descSize, colText, false, line)
	}
	text(b, x, 324, 7, 22, colSubtle, false,
		fmt.Sprintf("%s - %s", timeText(p.Start.In(f.Now.Location()), f.Clock24h),
			timeText(p.Stop.In(f.Now.Location()), f.Clock24h)))
}

// renderGrid draws the bottom half: time slots across, channels down.
func renderGrid(b *strings.Builder, f Frame) {
	start := f.Now.Truncate(slot)
	end := start.Add(Window)

	rect(b, 0, gridY, canvasW, headerH, colHeader)
	text(b, 14, gridY+headerH/2, 4, 22, colAccent, true, "CHANNEL")
	for i := 0; i < int(Window/slot); i++ {
		x := timeX + i*timeW/int(Window/slot)
		text(b, x+10, gridY+headerH/2, 4, 22, colAccent, true, timeText(start.Add(time.Duration(i)*slot), f.Clock24h))
	}

	if len(f.Channels) == 0 {
		text(b, canvasW/2, gridY+headerH+rowH*RowsPerPage/2, 5, 30, colSubtle, true, "NO CHANNELS")
		return
	}

	pages := Pages(len(f.Channels))
	page := ((f.Page % pages) + pages) % pages
	first := page * RowsPerPage
	for row := 0; row < RowsPerPage && first+row < len(f.Channels); row++ {
		ch := f.Channels[first+row]
		y := gridY + headerH + row*rowH
		renderRow(b, ch, y, ch.Number == f.Playing, start, end, f.Clock24h)
	}
}

func renderRow(b *strings.Builder, ch ersatztv.GuideChannel, y int, playing bool, start, end time.Time, h24 bool) {
	chanFill, chanText, progFill := colChannel, colText, colProgramme
	if playing {
		chanFill, chanText, progFill = colAccent, colBackground, colHighlight
	}

	rect(b, gap, y+gap, chanW-2*gap, rowH-2*gap, chanFill)
	text(b, 14, y+8, 7, 26, chanText, true, truncate(ch.Number, chars(chanW-28, 26)))
	text(b, 14, y+36, 7, 20, chanText, false, truncate(strings.ToUpper(ch.Name), chars(chanW-28, 20)))

	drawn := false
	for _, p := range ch.Programmes {
		if !p.Stop.After(start) || !p.Start.Before(end) {
			continue
		}
		from, to := maxTime(p.Start, start), minTime(p.Stop, end)
		x0 := timeX + int(float64(timeW)*from.Sub(start).Seconds()/Window.Seconds())
		x1 := timeX + int(float64(timeW)*to.Sub(start).Seconds()/Window.Seconds())
		if x1-x0 <= 2*gap {
			continue
		}
		rect(b, x0+gap, y+gap, x1-x0-2*gap, rowH-2*gap, progFill)
		title := p.Title
		if p.Start.Before(start) {
			// Began before the grid does, as the arrow on a real guide said.
			title = "◄ " + title
		}
		if n := chars(x1-x0-2*gap-16, 24); n >= 3 {
			text(b, x0+gap+8, y+rowH/2, 4, 24, colText, false, truncate(title, n))
		}
		drawn = true
	}
	if !drawn {
		rect(b, timeX+gap, y+gap, timeW-2*gap, rowH-2*gap, progFill)
		text(b, timeX+gap+8, y+rowH/2, 4, 24, colSubtle, false, "NO LISTINGS")
	}
}

// The Wi-Fi symbol: a dot and two arcs, the radius of the outermost -- which
// is also its height, chosen to match the clock's digits.
const wifiIconR = 23

// clockMidY is the vertical centre of the clock's digits on the canvas, as
// rendered: measured from a screenshot of the television, where the digits of
// DejaVu Sans Mono at size 34, positioned at y=16, span rows 22 to 42.
const clockMidY = 32

// colOffline slashes the Wi-Fi symbol when the Timeblaster is off the network.
const colOffline = "#FF4D5E"

// wifiIcon draws the Wi-Fi symbol with its point at (cx, cy): the familiar
// fan of arcs, in the accent colour when connected; dimmed, with a slash
// through it, when not. Drawn as shapes because the guide's font has no such
// glyph -- each arc is a quarter-circle wedge with the next one cut out of it
// in the background colour, which is how the bands appear.
func wifiIcon(b *strings.Builder, cx, cy int, connected bool) {
	col := colAccent
	if !connected {
		col = colSubtle
	}
	r := float64(wifiIconR)
	for i, rr := range []float64{r, r * 0.74, r * 0.52, r * 0.28, r * 0.14} {
		fill := col
		if i%2 == 1 {
			fill = colBackground
		}
		wedge(b, cx, cy, rr, fill)
	}
	if !connected {
		// A slash, outlined in the background so it reads against the arcs.
		x0, y0 := float64(cx)-r*0.85, float64(cy)+2
		x1, y1 := float64(cx)+r*0.85, float64(cy)-r-2
		bar(b, x0, y0, x1, y1, 7, colBackground)
		bar(b, x0, y0, x1, y1, 3.5, colOffline)
	}
}

// wedge fills a 90-degree sector pointing up, its tip at (cx, cy): two
// straight sides and a cubic Bezier for the arc.
func wedge(b *strings.Builder, cx, cy int, r float64, color string) {
	const s = 0.70710678 // sin and cos of 45 degrees
	const k = 0.55228475 // a quarter circle's Bezier handle, as a fraction of r
	x, y := float64(cx), float64(cy)
	p0x, p0y := x-r*s, y-r*s
	p3x, p3y := x+r*s, y-r*s
	p1x, p1y := p0x+k*r*s, p0y-k*r*s
	p2x, p2y := p3x-k*r*s, p3y-k*r*s
	fmt.Fprintf(b, "{\\an7\\pos(0,0)\\bord0\\shad0\\c%s\\p1}m %.1f %.1f l %.1f %.1f b %.1f %.1f %.1f %.1f %.1f %.1f l %.1f %.1f{\\p0}\n",
		assColor(color), x, y, p0x, p0y, p1x, p1y, p2x, p2y, p3x, p3y, x, y)
}

// bar fills a straight stroke of a width from one point to another.
func bar(b *strings.Builder, x0, y0, x1, y1, width float64, color string) {
	dx, dy := x1-x0, y1-y0
	n := math.Hypot(dx, dy)
	if n == 0 {
		return
	}
	ox, oy := -dy/n*width/2, dx/n*width/2
	fmt.Fprintf(b, "{\\an7\\pos(0,0)\\bord0\\shad0\\c%s\\p1}m %.1f %.1f l %.1f %.1f l %.1f %.1f l %.1f %.1f{\\p0}\n",
		assColor(color), x0+ox, y0+oy, x1+ox, y1+oy, x1-ox, y1-oy, x0-ox, y0-oy)
}

// --- drawing primitives ----------------------------------------------------

// rect fills a rectangle, as an ASS vector drawing on its own event line.
func rect(b *strings.Builder, x, y, w, h int, color string) {
	fmt.Fprintf(b, "{\\an7\\pos(%d,%d)\\bord0\\shad0\\c%s\\p1}m 0 0 l %d 0 l %d %d l 0 %d{\\p0}\n",
		x, y, assColor(color), w, w, h, h)
}

// text draws one line. align is an ASS numpad alignment: 7 top-left, 4
// middle-left, 5 centre.
func text(b *strings.Builder, x, y, align, size int, color string, bold bool, s string) {
	weight := 0
	if bold {
		weight = 1
	}
	fmt.Fprintf(b, "{\\an%d\\pos(%d,%d)\\fn%s\\fs%d\\b%d\\c%s\\bord0\\shad0}%s\n",
		align, x, y, font, size, weight, assColor(color), mpv.EscapeASS(s))
}

// assColor renders #RRGGBB as ASS's &HBBGGRR&.
func assColor(hex string) string {
	h := strings.TrimPrefix(hex, "#")
	if len(h) != 6 {
		return "&HFFFFFF&"
	}
	return "&H" + strings.ToUpper(h[4:6]+h[2:4]+h[0:2]) + "&"
}

// chars is how many characters of a size fit in a width.
func chars(width, size int) int {
	if width <= 0 || size <= 0 {
		return 0
	}
	return int(float64(width) / (float64(size) * charWidth))
}

// wrap breaks s into at most maxLines lines of at most width characters, at
// word boundaries. Text that does not fit ends with "…" on the last line, cut
// at a word where it can be. The guide's font is fixed-width, so a character
// count is a width: there is no measuring to get wrong.
func wrap(s string, width, maxLines int) []string {
	words := strings.Fields(s)
	if len(words) == 0 || width <= 1 || maxLines <= 0 {
		return nil
	}
	var lines []string
	var cur []rune
	for _, w := range words {
		r := []rune(w)
		// A word longer than a whole line is split across lines.
		for len(r) > width {
			if len(cur) > 0 {
				lines = append(lines, string(cur))
				cur = nil
			}
			lines = append(lines, string(r[:width]))
			r = r[width:]
		}
		switch {
		case len(cur) == 0:
			cur = r
		case len(cur)+1+len(r) <= width:
			cur = append(append(cur, ' '), r...)
		default:
			lines = append(lines, string(cur))
			cur = r
		}
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	if len(lines) <= maxLines {
		return lines
	}

	// Too long: keep maxLines, and end the last with an ellipsis that fits.
	lines = lines[:maxLines]
	last := []rune(lines[maxLines-1])
	for len(last)+1 > width {
		if i := strings.LastIndex(string(last), " "); i > 0 {
			last = []rune(string(last)[:i])
		} else {
			last = last[:width-1]
		}
	}
	lines[maxLines-1] = strings.TrimRight(string(last), " ,;:-") + "…"
	return lines
}

// truncate shortens s to n characters, marking the cut.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return string(r[:1])
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// timeText renders a time in the grid: "9:30 PM" or "21:30".
func timeText(t time.Time, h24 bool) string {
	if h24 {
		return t.Format("15:04")
	}
	return t.Format("3:04 PM")
}

func find(chs []ersatztv.GuideChannel, number string) *ersatztv.GuideChannel {
	for i := range chs {
		if chs[i].Number == number {
			return &chs[i]
		}
	}
	return nil
}

func current(ps []ersatztv.Programme, now time.Time) *ersatztv.Programme {
	for i := range ps {
		if !ps[i].Start.After(now) && ps[i].Stop.After(now) {
			return &ps[i]
		}
	}
	return nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
