package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/ersatztv"
)

func TestTheGuideAPICarriesDescriptions(t *testing.T) {
	// The Guide tab shows each programme's synopsis; it can only show what
	// the API sends.
	f := newFixture(t)
	now := time.Now()
	f.media.mu.Lock()
	f.media.channels[0].GuideID = "C1"
	f.media.guide = ersatztv.XMLTV{Programmes: map[string][]ersatztv.Programme{"C1": {{
		Start: now.Add(-10 * time.Minute), Stop: now.Add(20 * time.Minute),
		Title: "The Joy of Painting", SubTitle: "Woodman's Retreat",
		Description: "Explore the gorgeous mountain countryside with Bob Ross.",
	}}}}
	f.media.mu.Unlock()

	rec := f.do(t, http.MethodGet, "/api/guide?hours=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"description":"Explore the gorgeous mountain countryside with Bob Ross."`) {
		t.Errorf("the description did not reach the app: %s", rec.Body)
	}
}
