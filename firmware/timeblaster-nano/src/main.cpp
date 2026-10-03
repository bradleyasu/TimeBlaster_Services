/*
 * Timeblaster — Arduino Nano ESP32 firmware
 * =========================================
 *
 * The Nano is the hardware I/O controller. It reads the potentiometers and
 * buttons, drives the 7-segment display and the LEDs, and reports what it sees
 * to the Raspberry Pi over USB serial. The Pi decides what any of it means.
 *
 * What this firmware deliberately does NOT do:
 *
 *   - decide that a given ADC value means "channel 4";
 *   - decide that a button held for five seconds means "Wi-Fi setup";
 *   - store an alarm schedule;
 *   - decide when to ring.
 *
 * All of that is application policy and lives in timeblasterd on the Pi, where
 * it can be changed, configured and unit-tested without a soldering iron. It is
 * why this file is as small as it is.
 *
 * Layout:
 *
 *   Pins.h            every pin assignment, in one place
 *   SevenSegment.*    the display driver, carried over unmodified from the
 *                     TimeblasterClock project — see README.md
 *   Display.*         what to show on the display
 *   Inputs.*          potentiometers, buttons, LEDs
 *   Protocol.*        the TB1 wire format
 *
 * Board: Arduino Nano ESP32 (ESP32-S3), built with PlatformIO.
 */

#include <Arduino.h>
#include <esp_system.h>

#include "Display.h"
#include "Inputs.h"
#include "Pins.h"
#include "Protocol.h"

#define FIRMWARE_VERSION "1.0.0"
#define HARDWARE_ID      "nano-esp32"

static const unsigned long SERIAL_BAUD = 115200;

// Liveness.
static const uint32_t PING_INTERVAL_MS = 2000;
// If the Pi stops answering (its service restarted, or USB was re-enumerated)
// the display keeps running from local time rather than freezing, and we
// re-announce ourselves so a restarted daemon picks us up promptly.
static const uint32_t PI_TIMEOUT_MS = 10000;

static uint32_t lastPiContact = 0;

// --- Transmit recovery ------------------------------------------------------
//
// Twice now the USB CDC transmit path has died while everything else kept
// running: the display updated, messages from the Pi still arrived, and nothing
// the Nano sent ever got out again. It stayed that way until someone pressed
// reset -- fifteen hours, the first time.
//
// The Nano cannot report this, because the report would travel the broken
// direction. It can detect it, though: the Pi keeps talking, and nothing leaves.
// A restart is the only cure that has ever worked, and it costs nothing -- the
// Nano holds no state the Pi does not push again on HELLO.
static const uint32_t TX_STALL_MS = 30000;
// Capped so a fault the restart cannot fix becomes a dead link rather than a
// display blinking off every half minute all night.
static const uint32_t MAX_TX_RECOVERIES = 3;
// Long enough to be sure the link really came back before handing the budget
// back, rather than counting the first PING after a restart as success.
static const uint32_t TX_HEALTHY_MS = 120000;

// Survives ESP.restart(), cleared on a power-on reset in setup(), so the cap
// counts consecutive failed recoveries rather than restarting forever.
RTC_DATA_ATTR static uint32_t txRecoveries = 0;

// --- Local clock ------------------------------------------------------------
//
// The Pi is the authoritative clock, but it only syncs once a minute. Between
// syncs the Nano advances its own time from millis(), so the display keeps
// ticking smoothly even if the Pi is briefly too busy to talk — which is
// exactly what happens while ErsatzTV is transcoding.

static bool     timeValid      = false;
static uint32_t syncUnix       = 0;   // unix seconds at the last sync
static int32_t  utcOffsetSec   = 0;
static uint32_t syncMillis     = 0;   // millis() at that moment
static uint16_t syncMillisFrac = 0;   // milliseconds into that second

static void applyTimeSync(uint32_t unixSec, int32_t offset, uint16_t millisFrac) {
  syncUnix       = unixSec;
  utcOffsetSec   = offset;
  syncMillisFrac = millisFrac;
  syncMillis     = millis();
  timeValid      = true;
  displaySetSynced(true);
}

// updateClockDisplay advances the displayed time from the free-running local
// clock. The display owns the colon blink; this only supplies hours and
// minutes.
static void updateClockDisplay() {
  if (!timeValid) {
    return;
  }

  uint32_t elapsed = millis() - syncMillis + syncMillisFrac;
  uint32_t nowUnix = syncUnix + (elapsed / 1000);
  int64_t  local   = (int64_t)nowUnix + utcOffsetSec;
  if (local < 0) {
    local = 0;
  }

  uint32_t secondsOfDay = (uint32_t)(local % 86400);
  uint8_t  hour24 = (uint8_t)(secondsOfDay / 3600);
  uint8_t  minute = (uint8_t)((secondsOfDay % 3600) / 60);

  // The Pi owns the 12/24-hour preference and pushes it over CONFIG; the
  // display applies it. Passing the raw hour keeps that policy in one place
  // instead of converting here and again there.
  displaySetTime(hour24, minute);
}

// --- Inbound messages -------------------------------------------------------

static void handleMessage(const Message& msg) {
  lastPiContact = millis();

  if (msg.type == "PONG") {
    return;   // liveness only
  }

  if (msg.type == "TIME" && msg.argc >= 2) {
    uint32_t unixSec = (uint32_t)strtoul(msg.args[0].c_str(), nullptr, 10);
    int32_t  offset  = (int32_t)strtol(msg.args[1].c_str(), nullptr, 10);
    uint16_t frac    = msg.argc >= 3 ? (uint16_t)strtoul(msg.args[2].c_str(), nullptr, 10) : 0;
    applyTimeSync(unixSec, offset, frac);
    return;
  }

  if (msg.type == "ALARM" && msg.argc >= 2 && msg.args[0] == "ACTIVE") {
    bool active = msg.args[1] == "1";
    displaySetAlarmActive(active);
    ledsSetAlarmActive(active);
    return;
  }

  if (msg.type == "DISPLAY" && msg.argc >= 1) {
    if (msg.args[0] == "TEXT" && msg.argc >= 2) {
      displaySetText(msg.args[1].c_str());
    } else if (msg.args[0] == "CLOCK") {
      displayShowClock();
    } else if (msg.args[0] == "BRIGHTNESS" && msg.argc >= 2) {
      displaySetBrightness((int)strtol(msg.args[1].c_str(), nullptr, 10));
    }
    return;
  }

  if (msg.type == "LED" && msg.argc >= 2) {
    ledsSet(msg.args[0], msg.args[1] == "1");
    return;
  }

  if (msg.type == "CONFIG" && msg.argc >= 2) {
    if (msg.args[0] == "pot_threshold") {
      inputsSetPotThreshold((uint16_t)strtol(msg.args[1].c_str(), nullptr, 10));
    } else if (msg.args[0] == "clock_24h") {
      displaySetClock24h(msg.args[1] == "1");
    }
    return;
  }

  // Unknown types are ignored rather than rejected, so a newer Pi can send
  // messages this firmware predates without provoking a stream of NAKs.
}

// --- Setup and loop ---------------------------------------------------------

void setup() {
  // RTC memory keeps its contents across a software restart but is undefined
  // after power-on, so the recovery count is only meaningful once anchored to a
  // reset that was not a cold boot.
  if (esp_reset_reason() == ESP_RST_POWERON) {
    txRecoveries = 0;
  }

  Serial.begin(SERIAL_BAUD);

  displayInit();
  inputsInit();

  // Give USB CDC a moment to enumerate, keeping the loading animation running
  // so the display is alive from the instant power is applied. The Pi answers
  // HELLO with a full resync, so a reset at any time recovers by itself.
  uint32_t start = millis();
  while (!Serial && (millis() - start) < 3000) {
    displayTick();
  }

  protocolSend2("HELLO", FIRMWARE_VERSION, HARDWARE_ID);
}

void loop() {
  protocolPoll(handleMessage);
  inputsPoll();

  uint32_t now = millis();

  // Liveness beacon.
  static uint32_t lastPing = 0;
  if (now - lastPing >= PING_INTERVAL_MS) {
    lastPing = now;
    // The dropped count rides along, so a transmit path that is discarding
    // everything is visible in the Pi's link status. It is zero in normal
    // operation, which is what makes a non-zero value worth looking at.
    protocolSend2("PING", String(now), String(protocolDroppedFrames()));
  }

  // Still being spoken to, but nothing getting out: restart rather than sit
  // mute until someone notices the knobs have stopped working.
  if (protocolTxStalled(now, lastPiContact, TX_STALL_MS, PI_TIMEOUT_MS)) {
    if (txRecoveries < MAX_TX_RECOVERIES) {
      txRecoveries++;
      displayTick();   // leave the display in a sane state on the way out
      ESP.restart();
    }
  } else if (txRecoveries > 0 && now > TX_HEALTHY_MS &&
             now - protocolLastTxOkMs() < PING_INTERVAL_MS * 2) {
    // Sending has worked steadily since the restart, so the next fault gets a
    // full budget again instead of inheriting an exhausted one.
    txRecoveries = 0;
  }

  // If the Pi has gone quiet, re-announce ourselves so a restarted daemon picks
  // us up promptly. The display keeps running from local time meanwhile.
  static uint32_t lastHello = 0;
  if (lastPiContact != 0 && (now - lastPiContact) > PI_TIMEOUT_MS) {
    if (now - lastHello > PI_TIMEOUT_MS) {
      lastHello = now;
      protocolSend2("HELLO", FIRMWARE_VERSION, HARDWARE_ID);
    }
  }

  updateClockDisplay();
  ledsTick();
  displayTick();
}
