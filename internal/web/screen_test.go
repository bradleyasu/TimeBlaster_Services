package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bradsheets/timeblaster/internal/screen"
)

func TestChoosingATVChannelInAnotherModeSaysHowToGetBack(t *testing.T) {
	// The mode knob is authoritative. A TV request while it points elsewhere
	// is a conflict the user can resolve, not a failure of the television.
	f := newFixture(t)
	f.media.selectErr = screen.ErrInactive
	rec := f.do(t, http.MethodPost, "/api/channels/select", `{"number":"1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "mode knob") {
		t.Errorf("the error does not say how to get back to TV: %s", rec.Body)
	}
}
