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

func TestAlarmEditorUsesANativeTimePicker(t *testing.T) {
	// The alarm model stores hours as 0-23, and a user in 12-hour mode should
	// never have to know that 7pm is 19. A pair of number fields plus an AM/PM
	// toggle did that job and summoned a keyboard; a native time input does it
	// with the platform's own wheel and cannot express an impossible time.
	html := readStatic(t, "index.html")
	tag := regexp.MustCompile(`<input[^>]*id="ed-time"[^>]*>`).FindString(html)
	if tag == "" {
		t.Fatal("the alarm editor has no ed-time input")
	}
	if !strings.Contains(tag, `type="time"`) {
		t.Errorf("the alarm time field is not a native time input, so it raises a keyboard: %s", tag)
	}

	// The old controls must be gone rather than merely hidden: two ways to set
	// one time is how they drift apart.
	for _, gone := range []string{`id="ed-hour"`, `id="ed-minute"`, `id="ed-meridiem"`} {
		if strings.Contains(html, gone) {
			t.Errorf("%s is still in the markup alongside the time picker", gone)
		}
	}

	js := readStatic(t, "app.js")
	for _, fn := range []string{"function setEditorTime", "function editorTime"} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js is missing %s, so the editor cannot convert between forms", fn)
		}
	}

	// Reading the time must still be able to fail. The picker cannot produce an
	// impossible time, but it can be left empty, and a browser without support
	// falls back to a text box that accepts anything. Defaulting to an hour
	// nobody chose is indistinguishable from the alarm having worked.
	if !strings.Contains(js, "time.error") {
		t.Error("saveAlarm does not check for a rejected time; is it assuming a default again?")
	}
	if !strings.Contains(funcBody(t, js, "editorTime"), "field:") {
		t.Error("editorTime never reports a rejected field, so nothing can be marked invalid")
	}
}

func TestEverySwitchHasItsTrackImmediatelyAfterTheInput(t *testing.T) {
	// The switch is styled through `.switch input:checked + span`, an adjacent
	// sibling selector. The span has to come immediately after the input or the
	// control renders but never visibly changes state -- it looks dead rather
	// than broken, which is the kind of failure that survives review.
	html := readStatic(t, "index.html")

	// Counting both ways matters. Matching only the well-formed shape lets a
	// malformed switch escape the check entirely rather than fail it, which is
	// exactly what happened the first time this test was written.
	declared := strings.Count(html, `class="switch"`)
	if declared == 0 {
		t.Fatal("no switches found in index.html; has the markup changed?")
	}
	wellFormed := regexp.MustCompile(
		`<span class="switch"><input[^>]*><span></span></span>`).FindAllString(html, -1)
	if len(wellFormed) != declared {
		t.Errorf("%d switch(es) declared but only %d have their track directly "+
			"after the input; the rest will render but never change state",
			declared, len(wellFormed))
	}

	css := readStatic(t, "app.css")
	if !strings.Contains(css, ".switch input:checked + span") {
		t.Error("app.css no longer styles the switch through the adjacent sibling")
	}
}

func TestSettingsTogglesUseTheSwitch(t *testing.T) {
	// Bare checkboxes next to the alarm list's switches looked like two
	// different applications.
	html := readStatic(t, "index.html")
	for _, id := range []string{"set-clock24", "set-display-on", "set-overlay"} {
		want := `<span class="switch"><input type="checkbox" id="` + id + `">`
		if !strings.Contains(html, want) {
			t.Errorf("%s is not wrapped in a switch", id)
		}
	}
}

func TestEverySettingsControlSavesItself(t *testing.T) {
	// There is no save button any more, so a control that nobody listens to is
	// a setting the user can change and lose. Nothing errors in that case --
	// the control moves, and the value silently never reaches the device.
	html := readStatic(t, "index.html")
	js := readStatic(t, "app.js")

	if strings.Contains(html, "btn-save-settings") || strings.Contains(js, "btn-save-settings") {
		t.Error("the save button is gone from the markup but still referenced")
	}

	// Every control in the settings panel, by id.
	panel := html[strings.Index(html, `id="tab-settings"`):]
	if end := strings.Index(panel, "</section>"); end > 0 {
		panel = panel[:end]
	}
	ids := regexp.MustCompile(`id="(set-[a-z0-9-]+)"`).FindAllStringSubmatch(panel, -1)
	if len(ids) == 0 {
		t.Fatal("found no settings controls; has the markup changed?")
	}

	for _, m := range ids {
		id := m[1]
		// Either in the change-listener list, or wired on its own line.
		listed := strings.Contains(js, "'"+id+"'")
		if !listed {
			t.Errorf("%s is in the settings panel but never referenced in app.js, "+
				"so changing it would be silently discarded", id)
		}
		if !strings.Contains(js, "addEventListener('change'") {
			t.Fatal("no change listeners are wired at all")
		}
	}
}
