#include "Display.h"
#include "Pins.h"
#include "SevenSegment.h"

#include <stdio.h>
#include <string.h>

// How the display decides what to show, in priority order:
//
//   1. brightness 0          -> blank
//   2. not yet time-synced   -> the driver's loading animation
//   3. an override text set  -> that text (scrolling if it is too long)
//   4. otherwise             -> the clock, colon blinking on the half second
//
// An active alarm flashes whatever 3 or 4 produced, so the display pulses in
// time with the alarm LED.

static const unsigned long COLON_PERIOD_MS = 1000;  // on for the first half
static const unsigned long ALARM_FLASH_MS  = 400;   // matches the alarm LED

static bool displayModeClock = true;
static char overrideText[24] = {0};

static int  clockHour   = 0;    // 1-12; 0 means "no time yet"
static int  clockMinute = 0;

static bool alarmActive   = false;
static bool timeSynced    = false;
static int  brightnessPct = 75;

// lastPushed guards against rebuilding the same string every pass. The driver
// already ignores unchanged text, so this is only about skipping the snprintf.
static char lastPushed[24] = {0};

static void push(const char* text) {
  if (strncmp(lastPushed, text, sizeof(lastPushed) - 1) == 0) {
    return;
  }
  strncpy(lastPushed, text, sizeof(lastPushed) - 1);
  lastPushed[sizeof(lastPushed) - 1] = '\0';
  setDisplayText(text);
}

void displayInit() {
  // Reset every piece of state this file owns. Without this, a re-init would
  // inherit whatever the previous run left behind — which is how the host tests
  // caught a stale "already synced" flag leaving the loading animation stuck on.
  displayModeClock = true;
  overrideText[0] = '\0';
  clockHour = 0;
  clockMinute = 0;
  alarmActive = false;
  timeSynced = false;
  brightnessPct = 75;
  lastPushed[0] = '\0';

  initializeSevenSegmentDisplay(DISPLAY_DIGIT_COUNT);

  if (DISPLAY_OE_PIN >= 0) {
    pinMode((uint8_t)DISPLAY_OE_PIN, OUTPUT);
    // ~OE is active low: 0 duty is fully on.
    analogWrite((uint8_t)DISPLAY_OE_PIN, 0);
  }

  // Nothing useful to show until the Pi sends a time, so spin.
  setLoading(true);
}

void displaySetSynced(bool synced) {
  if (synced == timeSynced) {
    return;
  }
  timeSynced = synced;

  if (synced) {
    // Hand the display back to real content. Clearing lastPushed forces the
    // next tick to push, because the driver's own content is whatever was set
    // before the animation took over.
    setLoading(false);
    lastPushed[0] = '\0';
  } else {
    setLoading(true);
  }
}

void displaySetTime(int hour12, int minute) {
  if (hour12 < 0)  hour12 = 0;
  if (hour12 > 12) hour12 = 12;
  if (minute < 0)  minute = 0;
  if (minute > 59) minute = 59;

  clockHour = hour12;
  clockMinute = minute;
}

void displayShowClock() {
  displayModeClock = true;
  overrideText[0] = '\0';
}

void displaySetText(const char* text) {
  if (text == nullptr) {
    displayShowClock();
    return;
  }
  displayModeClock = false;
  strncpy(overrideText, text, sizeof(overrideText) - 1);
  overrideText[sizeof(overrideText) - 1] = '\0';
}

void displaySetBrightness(int percent) {
  if (percent < 0)   percent = 0;
  if (percent > 100) percent = 100;
  brightnessPct = percent;

  if (DISPLAY_OE_PIN >= 0) {
    // ~OE is active low, so a higher duty cycle means a dimmer display.
    int duty = 255 - (percent * 255 / 100);
    analogWrite((uint8_t)DISPLAY_OE_PIN, duty);
  }
  // Without ~OE wired there is nothing to do here: displayTick() blanks the
  // display when the percentage is 0, and that is the whole of what this
  // hardware can honour.
}

void displaySetAlarmActive(bool active) {
  alarmActive = active;
  if (!active) {
    // Make sure a flash that happened to be mid-blank does not stick.
    lastPushed[0] = '\0';
  }
}

// buildClockText renders the time as four cells. The decimal point after the
// hours stands in for the colon, because the HDSP-K511 has a point per digit
// and no separate colon. A leading space rather than a leading zero reads far
// better on a clock: " 9.30", not "09.30".
static void buildClockText(char* out, size_t n, bool colonOn) {
  if (clockHour <= 0) {
    strncpy(out, "----", n - 1);
    out[n - 1] = '\0';
    return;
  }
  if (colonOn) {
    snprintf(out, n, "%2d.%02d", clockHour, clockMinute);
  } else {
    snprintf(out, n, "%2d%02d", clockHour, clockMinute);
  }
}

void displayTick() {
  unsigned long now = millis();

  // The display being off wins over everything else, including the alarm flash
  // and the waiting animation. Off means off.
  if (brightnessPct == 0) {
    setLoading(false);
    push("    ");
    paintDisplay();
    return;
  }

  if (!timeSynced) {
    // Waiting for the Raspberry Pi's first time sync: the Nano is powered from
    // the Pi's USB, so this covers the whole of the Pi's boot. The animation is
    // a far better "still coming up" indicator than a frozen or blank display.
    //
    // setLoading is idempotent, so asserting it here every tick costs nothing
    // and covers the case where the display was switched off and back on before
    // the first sync arrived — the off path stops the animation, and without
    // this the display would come back showing stale content.
    setLoading(true);
    paintDisplay();
    return;
  }

  // An active alarm flashes the display in time with the alarm LED.
  if (alarmActive && ((now / ALARM_FLASH_MS) % 2) == 1) {
    push("    ");
    paintDisplay();
    return;
  }

  if (!displayModeClock) {
    push(overrideText);
    paintDisplay();
    return;
  }

  char text[16];
  bool colonOn = (now % COLON_PERIOD_MS) < (COLON_PERIOD_MS / 2);
  buildClockText(text, sizeof(text), colonOn);
  push(text);

  paintDisplay();
}
