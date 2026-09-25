package web

import (
	"regexp"
	"strings"
	"testing"
)

// The companion app shows and hides everything -- panels, the ringing banner,
// the toast, the alarm editor -- with the `hidden` attribute. That only works
// if `hidden` actually wins.
//
// The browser's own rule is `[hidden] { display: none }`. Its specificity is
// (0,1,0), which merely *ties* with any class selector, and an author
// stylesheet beats the user agent on a tie. So a single `.sheet { display:
// flex }` was enough to pin the New Alarm sheet permanently open: the markup's
// `hidden` did nothing, and Cancel -- which only sets `hidden = true` -- had no
// visible effect either.

var (
	hiddenTagRe = regexp.MustCompile(`<[a-zA-Z][^>]*\bhidden\b[^>]*>`)
	classAttrRe = regexp.MustCompile(`class="([^"]*)"`)
	overrideRe  = regexp.MustCompile(`\[hidden\][^{]*\{[^}]*display\s*:\s*none\s*!important`)
)

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := embeddedStatic.ReadFile("static/" + name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// classSetsDisplay reports whether any rule for .class sets the display
// property, which is what defeats the `hidden` attribute.
func classSetsDisplay(css, class string) bool {
	re := regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `\s*\{([^}]*)\}`)
	for _, m := range re.FindAllStringSubmatch(css, -1) {
		if strings.Contains(m[1], "display") {
			return true
		}
	}
	return false
}

func TestHiddenAttributeIsNotDefeatedByClassDisplayRules(t *testing.T) {
	html := readStatic(t, "index.html")
	css := readStatic(t, "app.css")

	// Every class sitting on an element the app hides with `hidden`.
	classes := map[string]bool{}
	for _, tag := range hiddenTagRe.FindAllString(html, -1) {
		m := classAttrRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		for _, c := range strings.Fields(m[1]) {
			classes[c] = true
		}
	}
	if len(classes) == 0 {
		t.Fatal("found no hidden-able elements in index.html; has the markup changed?")
	}

	var collide []string
	for c := range classes {
		if classSetsDisplay(css, c) {
			collide = append(collide, "."+c)
		}
	}

	if !overrideRe.MatchString(css) {
		t.Errorf("app.css has no `[hidden] { display: none !important }` rule.\n"+
			"These classes sit on hidden-able elements and set display, so without it "+
			"the hidden attribute is silently ignored for them: %v", collide)
	}
}

func TestEditorSheetIsHiddenInTheMarkup(t *testing.T) {
	// The alarm editor must not be on screen when the page loads.
	html := readStatic(t, "index.html")
	re := regexp.MustCompile(`<div[^>]*id="editor"[^>]*>`)
	tag := re.FindString(html)
	if tag == "" {
		t.Fatal(`no element with id="editor" in index.html`)
	}
	if !strings.Contains(tag, "hidden") {
		t.Errorf("the alarm editor must start hidden, got: %s", tag)
	}
}

func TestAppJSDoesNotReadStateKeysTheServerNeverSends(t *testing.T) {
	// The TV guide formatted its times from state.settings.clock_24h. The state
	// document has no "settings" key at all -- the preference lives under
	// state.clock -- so the guide silently ignored 24-hour mode. Nothing throws
	// on a missing key in JavaScript; it just quietly does the wrong thing,
	// which is why this is worth pinning rather than leaving to review.
	js := readStatic(t, "app.js")
	for _, absent := range []string{"state.settings"} {
		if strings.Contains(js, absent) {
			t.Errorf("app.js reads %q, which /api/state does not provide", absent)
		}
	}
}

func TestAlarmEditorHasAMeridiemControl(t *testing.T) {
	// The alarm model stores hours as 0-23. Without this control a user in
	// 12-hour mode had to know that 7pm is 19, which the rest of the app never
	// asks of them.
	html := readStatic(t, "index.html")
	if !strings.Contains(html, `id="ed-meridiem"`) {
		t.Error("the alarm editor has no AM/PM control")
	}
	js := readStatic(t, "app.js")
	for _, fn := range []string{"function setEditorTime", "function editorTime"} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js is missing %s, so the editor cannot convert between forms", fn)
		}
	}

	// Reading the time must be able to fail. Clamping an impossible hour into
	// range silently saved an alarm for a time nobody asked for.
	if !strings.Contains(js, "time.error") {
		t.Error("saveAlarm does not check for a rejected time; is it clamping again?")
	}
}
