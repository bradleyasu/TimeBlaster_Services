package app

import (
	"context"
	"time"
)

// shutdownAnswerDelay lets the ACCEPTED answer reach the Nano before services
// begin stopping. systemctl returns at once and the daemon is among the first
// things stopped, so without it the Nano could be left holding the loading
// animation waiting for an answer that was never sent -- and after a few seconds
// it would give up and show the clock while the Pi was in fact shutting down.
const shutdownAnswerDelay = 500 * time.Millisecond

// shutdownTimeout bounds the call to the helper.
const shutdownTimeout = 30 * time.Second

// ShutdownRequested handles the big red button's shutdown request.
//
// The Nano has already timed the ten-second hold, shown the countdown and
// confirmed the button was still down at the end. This is the second opinion,
// and it can only say no for reasons the Nano cannot know about.
func (a *App) ShutdownRequested() {
	// The Nano refuses to start a countdown during a ringing alarm, but it learns
	// that the alarm is ringing over the same link, a moment late. Checked again
	// here, against the scheduler itself.
	if a.scheduler.IsRinging() {
		a.log.Warn("shutdown requested from the button while an alarm is ringing; refusing")
		a.answerShutdown(false, "alarm ringing")
		return
	}
	if a.wifi == nil {
		a.log.Warn("shutdown requested from the button, but there is no privileged helper; refusing")
		a.answerShutdown(false, "no helper")
		return
	}

	// Loud on purpose: this is the line someone reads to find out why the
	// Timeblaster went off, and the journal is persistent now.
	a.log.Warn("shutdown requested from the big red button; powering off")
	a.answerShutdown(true, "")

	go func() {
		wait := a.clock.NewTimer(shutdownAnswerDelay)
		<-wait.C()

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := a.wifi.PowerOff(ctx); err != nil {
			a.log.Error("could not power off", "error", err)
			// The Nano is showing the animation as though it worked. Tell it
			// otherwise, or it would sit there until the Pi went quiet -- which,
			// with the Pi still running, it never would.
			a.answerShutdown(false, "power off failed")
		}
	}()
}

func (a *App) answerShutdown(accepted bool, reason string) {
	if a.nano == nil {
		return
	}
	if err := a.nano.AnswerShutdown(accepted, reason); err != nil {
		a.log.Warn("could not answer the shutdown request", "error", err)
	}
}
