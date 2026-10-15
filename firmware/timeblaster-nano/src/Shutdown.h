#ifndef TIMEBLASTER_SHUTDOWN_H
#define TIMEBLASTER_SHUTDOWN_H

#include <Arduino.h>

// Shutting the Timeblaster down by holding the big red button.
//
// This is the one piece of application policy the firmware owns, and it owns it
// for safety rather than convenience. If the Pi timed the hold from the press and
// release messages, a single lost release -- and this link has dropped frames --
// would leave the Pi believing the button was still down, and an ordinary press
// would power the device off ten seconds later. Here the button, the timer and
// the display are all local: letting go cancels instantly, and nothing at all is
// sent until the countdown has run out. The only message is one request, so the
// worst a lost frame can do is fail to shut down.
//
// The Pi still decides. It can refuse -- an alarm is ringing, say -- and the
// display goes back to normal.

enum ShutdownPhase {
  SHUTDOWN_IDLE,       // normal operation
  SHUTDOWN_HOLDING,    // the button is down and being timed
  SHUTDOWN_REQUESTED,  // the countdown ran out; waiting for the Pi to answer
  SHUTDOWN_CONFIRMED,  // the Pi is shutting down
  SHUTDOWN_OFF,        // the Pi has gone quiet: safe to unplug
};

// How long the button must be held, and when the countdown first appears.
// Nothing shows for the first two seconds, so an ordinary press -- dismissing an
// alarm -- never puts a digit on the display.
static const uint32_t SHUTDOWN_HOLD_MS       = 10000;
static const int      SHUTDOWN_SHOW_FROM     = 8;
// "0" is shown for this long before the request is sent, so it is seen at all.
static const uint32_t SHUTDOWN_ZERO_MS       = 500;
// No answer from the Pi in this long and the request is treated as lost.
static const uint32_t SHUTDOWN_ANSWER_MS     = 5000;
// After the Pi agrees, this much silence means it has finished. Only reached
// when the board keeps USB powered after halting; otherwise the Nano loses power
// first, and the display going dark is the signal.
static const uint32_t SHUTDOWN_SILENT_MS     = 30000;
// Readings before this are not trusted to show a release: the button module
// starts out reporting "up" until its debounce settles, which is exactly what a
// button held down since power-on looks like for the first few milliseconds.
static const uint32_t SHUTDOWN_SETTLE_MS     = 200;

void shutdownInit();

// shutdownTick advances the state machine. buttonDown is the big red button's
// debounced state, alarmActive whether an alarm is ringing, and lastPiContact
// when the Pi was last heard from (0 if never).
void shutdownTick(uint32_t now, bool buttonDown, bool alarmActive, uint32_t lastPiContact);

// shutdownPoll is what loop() calls. It reads the big red button and the alarm
// state itself, so which button can shut the Timeblaster down is decided here,
// where the host tests can see it, rather than in main.cpp, where they cannot.
// Only ALARM_OFF counts; the Wi-Fi button's hold belongs to the Pi.
void shutdownPoll(uint32_t now, uint32_t lastPiContact);

// shutdownAnswer applies the Pi's reply to a request.
void shutdownAnswer(bool accepted, uint32_t now);

ShutdownPhase shutdownPhase();

#endif // TIMEBLASTER_SHUTDOWN_H
