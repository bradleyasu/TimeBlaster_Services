package media

import (
	"context"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/ersatztv"
)

func schedule(start, stop time.Time) ersatztv.XMLTV {
	return ersatztv.XMLTV{
		Programmes: map[string][]ersatztv.Programme{"C1": {{Title: "Show", Start: start, Stop: stop}}},
		Numbers:    map[string]string{"C1": "1"},
	}
}

func TestACurrentScheduleIsKeptForTheTTL(t *testing.T) {
	f := newFixture(t, chans()...)
	now := f.clock.Now()
	f.etv.SetGuide(schedule(now.Add(-time.Hour), now.Add(time.Hour)))
	ctx := context.Background()

	_, _ = f.svc.Guide(ctx, time.Hour)
	f.clock.Advance(GuideTTL - time.Second)
	_, _ = f.svc.Guide(ctx, time.Hour)
	if n := f.etv.GuideReads(); n != 1 {
		t.Errorf("a current schedule was fetched %d times within the TTL", n)
	}
	f.clock.Advance(2 * time.Second)
	_, _ = f.svc.Guide(ctx, time.Hour)
	if n := f.etv.GuideReads(); n != 2 {
		t.Errorf("not re-fetched after the TTL: %d reads", n)
	}
}

func TestAScheduleEntirelyInThePastIsFetchedAgainSoon(t *testing.T) {
	// What ErsatzTV serves for the first minute after starting, when the
	// Timeblaster has been off for days: the schedule it saved last time.
	f := newFixture(t, chans()...)
	now := f.clock.Now()
	f.etv.SetGuide(schedule(now.Add(-120*time.Hour), now.Add(-119*time.Hour)))
	ctx := context.Background()
	if err := f.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	_, _ = f.svc.Guide(ctx, time.Hour)
	f.etv.SetGuide(schedule(now.Add(-time.Minute), now.Add(time.Hour))) // rebuilt
	f.clock.Advance(GuideStaleRetry)
	g, _ := f.svc.Guide(ctx, time.Hour)
	if n := f.etv.GuideReads(); n != 2 {
		t.Fatalf("a stale schedule was held: %d reads after %s", n, GuideStaleRetry)
	}
	if len(g.Channels) == 0 || len(g.Channels[0].Programmes) != 1 {
		t.Errorf("the rebuilt schedule was not served: %+v", g.Channels)
	}
}
