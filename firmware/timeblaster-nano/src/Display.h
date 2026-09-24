#ifndef TIMEBLASTER_DISPLAY_H
#define TIMEBLASTER_DISPLAY_H

#include <Arduino.h>

// The Timeblaster's view of the 7-segment display.
//
// This sits on top of SevenSegment.{h,cpp}, which is the working driver carried
// over unmodified from the TimeblasterClock project. Everything here is about
// *what* to show; that file owns *how* to show it.
//
// The driver underneath is retained-mode: you say what should be on the display
// and call displayTick() every pass of loop(). Pushing the same content again
// is free, so nothing here has to track whether an update is needed.

void displayInit();

// displayTick paints the display and advances the clock colon, the alarm flash
// and any scrolling text. Call it every pass of loop().
void displayTick();

// displayShowClock returns the display to showing the time.
void displayShowClock();

// displaySetTime updates the time the clock mode shows. Hours are 0-23; the
// 12/24-hour conversion happens here rather than at the call site, so the
// display owns its own formatting.
void displaySetTime(int hour24, int minute);

// displaySetClock24h selects 24-hour display. It follows the user's setting,
// which the Pi pushes over CONFIG and re-sends on every reconnect.
void displaySetClock24h(bool on);

// displayClock24h reports the current mode.
bool displayClock24h();

// displaySetText overrides the clock with literal text until displayShowClock()
// is called. Text longer than the four digits scrolls by itself.
void displaySetText(const char* text);

// displaySetBrightness takes 0-100.
//
// The 595s' ~OE is tied low on this board, so there is no hardware dimming
// unless DISPLAY_OE_PIN is wired: see Pins.h. Without it, 0 blanks the display
// and anything else turns it on, which is the honest subset of the protocol's
// BRIGHTNESS message that this hardware can actually honour.
void displaySetBrightness(int percent);

// displaySetAlarmActive starts or stops the ringing-alarm flash.
void displaySetAlarmActive(bool active);

// displaySetSynced tells the display whether the Pi has sent a time yet. Until
// it has, the driver's loading animation runs, which is a much better "waiting
// for the Raspberry Pi" indicator than four dashes.
void displaySetSynced(bool synced);

#endif // TIMEBLASTER_DISPLAY_H
