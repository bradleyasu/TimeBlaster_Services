#include "Shutdown.h"

#include <stdio.h>

#include "Display.h"
#include "Inputs.h"
#include "Protocol.h"

static ShutdownPhase phase = SHUTDOWN_IDLE;
static uint32_t pressedAt   = 0;
static uint32_t requestedAt = 0;

// seenRelease stays false until the button has been seen up after power-on. A
// button that is stuck, or held down by something resting on it, would otherwise
// shut the Timeblaster down ten seconds after every boot, with no way out.
static bool seenRelease = false;

// pressSpent marks the current press as unable to arm a shutdown: it began
// while an alarm was ringing, was interrupted by one, or already ended in a
// failed or refused request. Cleared only by letting go.
static bool pressSpent = false;

static void backToNormal() {
  phase = SHUTDOWN_IDLE;
  displayShutdownClear();
}

void shutdownInit() {
  phase = SHUTDOWN_IDLE;
  pressedAt = requestedAt = 0;
  seenRelease = false;
  pressSpent = false;
  displayShutdownClear();
}

ShutdownPhase shutdownPhase() { return phase; }

void shutdownTick(uint32_t now, bool buttonDown, bool alarmActive, uint32_t lastPiContact) {
  if (!buttonDown) {
    if (now >= SHUTDOWN_SETTLE_MS) {
      seenRelease = true;
    }
    pressSpent = false;
  }

  switch (phase) {
    case SHUTDOWN_IDLE:
      if (!buttonDown || !seenRelease || pressSpent) {
        return;
      }
      // A press that starts while an alarm rings is for silencing it, and
      // keeping it held must never turn into switching the alarm clock off.
      if (alarmActive) {
        pressSpent = true;
        return;
      }
      phase = SHUTDOWN_HOLDING;
      pressedAt = now;
      return;

    case SHUTDOWN_HOLDING: {
      if (!buttonDown) {
        backToNormal();
        return;
      }
      if (alarmActive) {
        pressSpent = true;
        backToNormal();
        return;
      }
      uint32_t held = now - pressedAt;
      if (held >= SHUTDOWN_HOLD_MS + SHUTDOWN_ZERO_MS) {
        protocolSend1("SHUTDOWN", "REQUEST");
        phase = SHUTDOWN_REQUESTED;
        requestedAt = now;
        displayShutdownAnimate();
        return;
      }
      int remaining = (int)(SHUTDOWN_HOLD_MS / 1000) - (int)(held / 1000);
      if (remaining < 0) {
        remaining = 0;
      }
      if (remaining <= SHUTDOWN_SHOW_FROM) {
        char text[8];
        snprintf(text, sizeof(text), "%4d", remaining);
        displayShutdownText(text);
      }
      return;
    }

    case SHUTDOWN_REQUESTED:
      // Letting go no longer matters. Silence does: the request may never have
      // left -- this transmit path has died before -- and an animation that
      // never ends would look like a shutdown that is happening.
      if (now - requestedAt >= SHUTDOWN_ANSWER_MS) {
        pressSpent = buttonDown;
        backToNormal();
      }
      return;

    case SHUTDOWN_CONFIRMED:
      if (lastPiContact != 0 && now - lastPiContact >= SHUTDOWN_SILENT_MS) {
        phase = SHUTDOWN_OFF;
        displayShutdownText(" OFF");
      }
      return;

    case SHUTDOWN_OFF:
      // Hearing from the Pi again means it came back up without the Nano losing
      // power -- someone pressed its power button. Back to work.
      if (lastPiContact != 0 && now - lastPiContact < SHUTDOWN_SILENT_MS) {
        backToNormal();
      }
      return;
  }
}

void shutdownPoll(uint32_t now, uint32_t lastPiContact) {
  shutdownTick(now, inputsButtonDown("ALARM_OFF"), displayAlarmIsActive(), lastPiContact);
}

void shutdownAnswer(bool accepted, uint32_t now) {
  if (phase == SHUTDOWN_REQUESTED) {
    if (accepted) {
      phase = SHUTDOWN_CONFIRMED;
      return;
    }
    pressSpent = true;
    backToNormal();
    return;
  }
  // Agreed, then the shutdown itself failed on the Pi.
  if (phase == SHUTDOWN_CONFIRMED && !accepted) {
    pressSpent = true;
    backToNormal();
  }
  (void)now;
}
