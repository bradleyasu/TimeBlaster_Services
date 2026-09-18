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
 * All of that is application policy and lives in timeblasterd on the Pi. Keeping
 * it there means it can be changed, configured and unit-tested without a
 * soldering iron, and it is why this file is as small as it is.
 *
 * What the firmware DOES own is everything that benefits from being close to the
 * hardware: oversampling and smoothing the noisy ESP32 ADC, debouncing the
 * buttons, multiplexing the display, and free-running the clock between time
 * syncs so the display never visibly freezes because Linux was briefly busy.
 *
 * Protocol: see docs/serial-protocol.md. The wire format is implemented twice —
 * here and in internal/protocol — and the Go side's tests pin the exact bytes,
 * so a change on either side that breaks compatibility fails a test rather than
 * a bench session.
 *
 * Board: Arduino Nano ESP32 (ESP32-S3). Select it in the IDE's board manager
 * before uploading.
 */

#include <Arduino.h>

// ---------------------------------------------------------------------------
// Build configuration
// ---------------------------------------------------------------------------

#define FIRMWARE_VERSION "1.0.0"
#define HARDWARE_ID      "nano-esp32"

// Serial link. USB CDC ignores the baud rate, but stating it documents the
// intent and keeps a future hard-wired UART working unchanged.
static const unsigned long SERIAL_BAUD = 115200;

// --- Potentiometers ---------------------------------------------------------
//
// Wiring, for each of the four 10k linear pots:
//   outer leg 1 -> 3V3
//   wiper       -> A0..A3
//   outer leg 2 -> GND
//
// Pot 0 = TV channel, Pot 1 = alarm/speaker volume, Pots 2 and 3 are reserved.
// Those assignments live on the Pi; this firmware only knows there are four.
static const uint8_t POT_PINS[] = {A0, A1, A2, A3};
static const uint8_t POT_COUNT  = sizeof(POT_PINS) / sizeof(POT_PINS[0]);

// The ESP32-S3 ADC is 12-bit and noticeably noisy, so each reported value is the
// mean of several samples passed through an exponential moving average. This is
// *smoothing*, not policy: the Pi still does calibration, dead zones, hysteresis
// and range mapping.
static const uint8_t  POT_OVERSAMPLE   = 16;
static const float    POT_EMA_ALPHA    = 0.25f;
static const uint16_t POT_SAMPLE_MS    = 20;

// A reading is only sent when it has moved by at least this much, or when
// POT_HEARTBEAT_MS has elapsed. This is what stops the serial link being flooded
// with every tiny ADC fluctuation; the threshold is well below anything a human
// hand produces, so deliberate movement is never missed.
static uint16_t potReportThreshold = 12;   // in raw ADC counts, tunable via CONFIG
static const uint32_t POT_HEARTBEAT_MS = 5000;

// --- Buttons ----------------------------------------------------------------
//
// Both buttons are wired pin -> GND and use the internal pull-up, so they read
// LOW when pressed. Only edges are reported; the Pi measures hold duration.
static const uint8_t BUTTON_WIFI_PIN      = D2;
static const uint8_t BUTTON_ALARM_OFF_PIN = D3;
static const uint16_t DEBOUNCE_MS         = 25;

// --- LEDs -------------------------------------------------------------------
//
// Named logically so the Pi never refers to a pin number.
static const uint8_t LED_ALARM_PIN = D4;
static const uint8_t LED_WIFI_PIN  = D5;
static const uint8_t LED_POWER_PIN = D6;

// --- 7-segment display ------------------------------------------------------
//
// A 4-digit common-cathode display driven directly: seven segment pins plus a
// decimal point, and four digit-enable pins. Swap in a TM1637 or MAX7219 by
// replacing only displayInit(), displayRefresh() and displaySetBrightness().
static const uint8_t SEG_PINS[8]   = {D7, D8, D9, D10, D11, D12, D13, A7};  // a,b,c,d,e,f,g,dp
static const uint8_t DIGIT_PINS[4] = {A4, A5, A6, D1};
static const bool    SEG_ACTIVE_HIGH   = true;   // common-cathode
static const bool    DIGIT_ACTIVE_HIGH = false;  // digit pulled low to enable

// Multiplex fast enough that no flicker is visible.
static const uint16_t DIGIT_DWELL_US = 2000;

// --- Liveness ---------------------------------------------------------------
static const uint32_t PING_INTERVAL_MS = 2000;
// If the Pi stops answering (its service restarted, or USB was re-enumerated)
// the display falls back to free-running local time rather than freezing.
static const uint32_t PI_TIMEOUT_MS = 10000;

// ---------------------------------------------------------------------------
// Protocol
// ---------------------------------------------------------------------------

static const char STX = 0x02;
static const char ETX = '\n';
static const char* PROTOCOL_VERSION = "TB1";
static const uint16_t MAX_FRAME_LEN = 512;
static const uint16_t MAX_SEQ = 4096;

// CRC-8, polynomial 0x07, init 0x00. Computed bitwise: a 256-byte table is not
// worth the RAM here, and frames are short.
static uint8_t crc8(const char* data, size_t len) {
  uint8_t crc = 0;
  for (size_t i = 0; i < len; i++) {
    crc ^= (uint8_t)data[i];
    for (uint8_t b = 0; b < 8; b++) {
      crc = (crc & 0x80) ? (uint8_t)((crc << 1) ^ 0x07) : (uint8_t)(crc << 1);
    }
  }
  return crc;
}

static uint16_t txSeq = 0;

// appendEscaped writes s into out, escaping the bytes that would break framing.
static void appendEscaped(String& out, const String& s) {
  for (size_t i = 0; i < s.length(); i++) {
    char c = s[i];
    switch (c) {
      case '\\': out += "\\\\"; break;
      case '|':  out += "\\p";  break;
      case '\n': out += "\\n";  break;
      case '\r': out += "\\r";  break;
      case STX:  out += "\\s";  break;
      default:   out += c;      break;
    }
  }
}

// sendFrame emits one complete TB1 frame.
static void sendFrame(const String& type, const String* args, uint8_t argc) {
  String body = String(PROTOCOL_VERSION);
  body += '|';
  body += String(txSeq % MAX_SEQ);
  body += '|';
  body += type;
  for (uint8_t i = 0; i < argc; i++) {
    body += '|';
    appendEscaped(body, args[i]);
  }
  txSeq = (txSeq + 1) % MAX_SEQ;

  char crcHex[3];
  snprintf(crcHex, sizeof(crcHex), "%02X", crc8(body.c_str(), body.length()));

  Serial.write(STX);
  Serial.print(body);
  Serial.write('|');
  Serial.print(crcHex);
  Serial.write(ETX);
}

static void sendFrame0(const String& type) { sendFrame(type, nullptr, 0); }

static void sendFrame1(const String& type, const String& a) {
  String args[1] = {a};
  sendFrame(type, args, 1);
}

static void sendFrame2(const String& type, const String& a, const String& b) {
  String args[2] = {a, b};
  sendFrame(type, args, 2);
}

// ---------------------------------------------------------------------------
// Local clock
// ---------------------------------------------------------------------------
//
// The Pi is the authoritative clock, but it only syncs once a minute. Between
// syncs the Nano advances its own time from millis(), so the display keeps
// ticking smoothly even if the Pi is briefly too busy to talk — which is exactly
// what happens when FFmpeg is transcoding.

static bool     timeValid     = false;
static uint32_t syncUnix      = 0;   // unix seconds at the moment of the last sync
static int32_t  utcOffsetSec  = 0;
static uint32_t syncMillis    = 0;   // millis() at that moment
static uint16_t syncMillisFrac = 0;  // milliseconds into that second
static uint32_t lastPiContact = 0;

static void applyTimeSync(uint32_t unixSec, int32_t offset, uint16_t millisFrac) {
  syncUnix       = unixSec;
  utcOffsetSec   = offset;
  syncMillisFrac = millisFrac;
  syncMillis     = millis();
  timeValid      = true;
}

// localNow returns seconds since midnight in local time, and the milliseconds
// into the current second.
static void localNow(uint32_t* secondsOfDay, uint16_t* millisOfSecond) {
  uint32_t elapsed = millis() - syncMillis + syncMillisFrac;
  uint32_t nowUnix = syncUnix + (elapsed / 1000);
  int64_t  local   = (int64_t)nowUnix + utcOffsetSec;
  if (local < 0) local = 0;
  *secondsOfDay   = (uint32_t)(local % 86400);
  *millisOfSecond = (uint16_t)(elapsed % 1000);
}

// ---------------------------------------------------------------------------
// Display
// ---------------------------------------------------------------------------

// Segment patterns, bit 0 = a … bit 6 = g.
static const uint8_t DIGIT_SEGMENTS[10] = {
  0b0111111, // 0
  0b0000110, // 1
  0b1011011, // 2
  0b1001111, // 3
  0b1100110, // 4
  0b1101101, // 5
  0b1111101, // 6
  0b0000111, // 7
  0b1111111, // 8
  0b1101111, // 9
};

// Enough letters for the status words the Pi sends: SETUP, ERR, WIFI, ON, OFF.
static uint8_t letterSegments(char c) {
  switch (toupper(c)) {
    case 'A': return 0b1110111;
    case 'B': return 0b1111100;
    case 'C': return 0b0111001;
    case 'D': return 0b1011110;
    case 'E': return 0b1111001;
    case 'F': return 0b1110001;
    case 'H': return 0b1110110;
    case 'I': return 0b0000110;
    case 'J': return 0b0011110;
    case 'L': return 0b0111000;
    case 'N': return 0b1010100;
    case 'O': return 0b0111111;
    case 'P': return 0b1110011;
    case 'R': return 0b1010000;
    case 'S': return 0b1101101;
    case 'T': return 0b1111000;
    case 'U': return 0b0111110;
    case 'Y': return 0b1101110;
    case '-': return 0b1000000;
    case ' ': return 0b0000000;
    default:  return 0b0000000;
  }
}

// displayBuffer holds the segment pattern for each digit, plus a decimal-point
// bit in position 7. It is the only thing displayRefresh reads, which keeps the
// multiplexing loop short.
static uint8_t displayBuffer[4] = {0, 0, 0, 0};
static uint8_t displayBrightness = 75;   // percent
static bool    displayShowClock  = true;
static bool    alarmActive       = false;
static char    displayText[5]    = "    ";

static void displayInit() {
  for (uint8_t i = 0; i < 8; i++) {
    pinMode(SEG_PINS[i], OUTPUT);
    digitalWrite(SEG_PINS[i], SEG_ACTIVE_HIGH ? LOW : HIGH);
  }
  for (uint8_t i = 0; i < 4; i++) {
    pinMode(DIGIT_PINS[i], OUTPUT);
    digitalWrite(DIGIT_PINS[i], DIGIT_ACTIVE_HIGH ? LOW : HIGH);
  }
}

static void displaySetText(const char* text) {
  for (uint8_t i = 0; i < 4; i++) {
    char c = text[i] ? text[i] : ' ';
    displayBuffer[i] = letterSegments(c);
    displayText[i] = c;
    if (!text[i]) break;
  }
  displayText[4] = '\0';
}

// displayRenderClock fills the buffer from the free-running local clock. The
// colon (the decimal point on digit 1) blinks on the half second, which is what
// makes a frozen display obvious at a glance.
static void displayRenderClock() {
  if (!timeValid) {
    displayBuffer[0] = letterSegments('-');
    displayBuffer[1] = letterSegments('-');
    displayBuffer[2] = letterSegments('-');
    displayBuffer[3] = letterSegments('-');
    return;
  }

  uint32_t secondsOfDay;
  uint16_t ms;
  localNow(&secondsOfDay, &ms);

  uint8_t hour24 = (uint8_t)(secondsOfDay / 3600);
  uint8_t minute = (uint8_t)((secondsOfDay % 3600) / 60);

  // 12-hour display: the Pi owns the 12/24 preference for the companion app, but
  // a 7-segment clock with no AM/PM indicator reads better in 12-hour form, and
  // the Pi can override the whole display with DISPLAY|TEXT if it disagrees.
  uint8_t hour = hour24 % 12;
  if (hour == 0) hour = 12;

  displayBuffer[0] = (hour >= 10) ? DIGIT_SEGMENTS[hour / 10] : 0;
  displayBuffer[1] = DIGIT_SEGMENTS[hour % 10];
  displayBuffer[2] = DIGIT_SEGMENTS[minute / 10];
  displayBuffer[3] = DIGIT_SEGMENTS[minute % 10];

  if (ms < 500) displayBuffer[1] |= 0x80;  // colon on
}

// displayRefresh lights one digit. Called continuously from loop().
static void displayRefresh() {
  static uint8_t digit = 0;
  static uint32_t lastSwitch = 0;

  uint32_t now = micros();
  // Brightness is pulse-width modulated across each digit's slot.
  uint32_t onTime = (uint32_t)DIGIT_DWELL_US * displayBrightness / 100;

  if (now - lastSwitch < DIGIT_DWELL_US) {
    if (now - lastSwitch > onTime) {
      digitalWrite(DIGIT_PINS[digit], DIGIT_ACTIVE_HIGH ? LOW : HIGH);  // blank
    }
    return;
  }
  lastSwitch = now;

  // Blank the previous digit before changing segments, or adjacent digits ghost.
  digitalWrite(DIGIT_PINS[digit], DIGIT_ACTIVE_HIGH ? LOW : HIGH);
  digit = (digit + 1) % 4;

  uint8_t pattern = displayBuffer[digit];
  for (uint8_t s = 0; s < 8; s++) {
    bool on = pattern & (1 << s);
    digitalWrite(SEG_PINS[s], SEG_ACTIVE_HIGH ? (on ? HIGH : LOW) : (on ? LOW : HIGH));
  }
  if (displayBrightness > 0) {
    digitalWrite(DIGIT_PINS[digit], DIGIT_ACTIVE_HIGH ? HIGH : LOW);
  }
}

// ---------------------------------------------------------------------------
// LEDs
// ---------------------------------------------------------------------------

static void ledsInit() {
  pinMode(LED_ALARM_PIN, OUTPUT);
  pinMode(LED_WIFI_PIN, OUTPUT);
  pinMode(LED_POWER_PIN, OUTPUT);
  digitalWrite(LED_POWER_PIN, HIGH);
}

static void setLED(const String& name, bool on) {
  if (name == "ALARM")      digitalWrite(LED_ALARM_PIN, on ? HIGH : LOW);
  else if (name == "WIFI")  digitalWrite(LED_WIFI_PIN, on ? HIGH : LOW);
  else if (name == "POWER") digitalWrite(LED_POWER_PIN, on ? HIGH : LOW);
}

// alarmEffects blinks the alarm LED and flashes the display while an alarm is
// ringing. This is presentation, not policy: the Pi decides that an alarm is
// active; the Nano decides how to make that visible.
static void alarmEffects() {
  if (!alarmActive) return;
  bool phase = (millis() / 400) % 2 == 0;
  digitalWrite(LED_ALARM_PIN, phase ? HIGH : LOW);
  if (!phase && displayShowClock) {
    for (uint8_t i = 0; i < 4; i++) displayBuffer[i] = 0;
  }
}

// ---------------------------------------------------------------------------
// Potentiometers
// ---------------------------------------------------------------------------

static float    potFiltered[POT_COUNT];
static uint16_t potLastSent[POT_COUNT];
static uint32_t potLastSentAt[POT_COUNT];
static bool     potPrimed[POT_COUNT];

static void potsInit() {
  analogReadResolution(12);
  for (uint8_t i = 0; i < POT_COUNT; i++) {
    potFiltered[i]   = 0;
    potLastSent[i]   = 0;
    potLastSentAt[i] = 0;
    potPrimed[i]     = false;
  }
}

static void potsSample() {
  static uint32_t lastSample = 0;
  uint32_t now = millis();
  if (now - lastSample < POT_SAMPLE_MS) return;
  lastSample = now;

  for (uint8_t i = 0; i < POT_COUNT; i++) {
    uint32_t sum = 0;
    for (uint8_t s = 0; s < POT_OVERSAMPLE; s++) sum += analogRead(POT_PINS[i]);
    float mean = (float)sum / POT_OVERSAMPLE;

    if (!potPrimed[i]) {
      // Take the first reading verbatim so the knob's position at boot is
      // reported immediately rather than ramped towards.
      potFiltered[i] = mean;
      potPrimed[i]   = true;
    } else {
      potFiltered[i] += POT_EMA_ALPHA * (mean - potFiltered[i]);
    }

    uint16_t value = (uint16_t)(potFiltered[i] + 0.5f);
    int32_t delta  = (int32_t)value - (int32_t)potLastSent[i];
    if (delta < 0) delta = -delta;

    bool moved    = (uint16_t)delta >= potReportThreshold;
    bool overdue  = (now - potLastSentAt[i]) >= POT_HEARTBEAT_MS;
    bool firstEver = potLastSentAt[i] == 0;

    if (moved || overdue || firstEver) {
      sendFrame2("POT", String(i), String(value));
      potLastSent[i]   = value;
      potLastSentAt[i] = now;
    }
  }
}

// ---------------------------------------------------------------------------
// Buttons
// ---------------------------------------------------------------------------

struct Button {
  uint8_t  pin;
  const char* name;
  bool     stable;        // debounced state: true when pressed
  bool     lastReading;
  uint32_t lastChange;
};

static Button buttons[] = {
  {BUTTON_WIFI_PIN,      "WIFI",      false, false, 0},
  {BUTTON_ALARM_OFF_PIN, "ALARM_OFF", false, false, 0},
};
static const uint8_t BUTTON_COUNT = sizeof(buttons) / sizeof(buttons[0]);

static void buttonsInit() {
  for (uint8_t i = 0; i < BUTTON_COUNT; i++) {
    pinMode(buttons[i].pin, INPUT_PULLUP);
    buttons[i].stable      = false;
    buttons[i].lastReading = false;
    buttons[i].lastChange  = 0;
  }
}

// buttonsPoll debounces and reports edges only. The five-second hold that enters
// Wi-Fi setup mode is measured on the Pi, where its duration is configurable and
// its behaviour is unit-tested.
static void buttonsPoll() {
  uint32_t now = millis();
  for (uint8_t i = 0; i < BUTTON_COUNT; i++) {
    bool reading = digitalRead(buttons[i].pin) == LOW;  // active low

    if (reading != buttons[i].lastReading) {
      buttons[i].lastReading = reading;
      buttons[i].lastChange  = now;
      continue;
    }
    if (reading != buttons[i].stable && (now - buttons[i].lastChange) >= DEBOUNCE_MS) {
      buttons[i].stable = reading;
      sendFrame2("BUTTON", String(buttons[i].name), reading ? "DOWN" : "UP");
    }
  }
}

// ---------------------------------------------------------------------------
// Inbound frames
// ---------------------------------------------------------------------------

static char rxBuffer[MAX_FRAME_LEN];
static uint16_t rxLen = 0;
static bool rxInFrame = false;

// unescapeField reverses appendEscaped.
static String unescapeField(const char* s, uint16_t len) {
  String out;
  out.reserve(len);
  for (uint16_t i = 0; i < len; i++) {
    if (s[i] == '\\' && i + 1 < len) {
      i++;
      switch (s[i]) {
        case '\\': out += '\\'; break;
        case 'p':  out += '|';  break;
        case 'n':  out += '\n'; break;
        case 'r':  out += '\r'; break;
        case 's':  out += STX;  break;
        default:   out += s[i]; break;
      }
    } else {
      out += s[i];
    }
  }
  return out;
}

static void handleMessage(const String& type, String* args, uint8_t argc);

// processFrame validates and dispatches one frame body.
static void processFrame(char* body, uint16_t len) {
  if (len == 0) return;

  // Split on unescaped separators.
  static const uint8_t MAX_FIELDS = 10;
  char* starts[MAX_FIELDS];
  uint16_t lens[MAX_FIELDS];
  uint8_t count = 0;

  uint16_t fieldStart = 0;
  for (uint16_t i = 0; i <= len && count < MAX_FIELDS; i++) {
    if (i < len && body[i] == '\\') { i++; continue; }
    if (i == len || body[i] == '|') {
      starts[count] = body + fieldStart;
      lens[count]   = i - fieldStart;
      count++;
      fieldStart = i + 1;
    }
  }
  if (count < 4) return;  // version | seq | type | crc

  // Version.
  if (lens[0] != 3 || strncmp(starts[0], PROTOCOL_VERSION, 3) != 0) return;

  // Checksum covers everything before the separator preceding the CRC field.
  uint16_t covered = len - lens[count - 1] - 1;
  char expected[3];
  snprintf(expected, sizeof(expected), "%02X", crc8(body, covered));
  if (lens[count - 1] != 2 || strncasecmp(starts[count - 1], expected, 2) != 0) return;

  String type = unescapeField(starts[2], lens[2]);
  String args[MAX_FIELDS];
  uint8_t argc = 0;
  for (uint8_t i = 3; i < count - 1 && argc < MAX_FIELDS; i++) {
    args[argc++] = unescapeField(starts[i], lens[i]);
  }

  lastPiContact = millis();
  handleMessage(type, args, argc);
}

// serialPoll reads bytes, framing on STX…newline. Anything before an STX is
// discarded, which is what lets the link resynchronise after a reset or a
// partially transmitted frame instead of staying permanently out of step.
static void serialPoll() {
  while (Serial.available() > 0) {
    char c = (char)Serial.read();

    if (c == STX) {
      rxInFrame = true;
      rxLen = 0;
      continue;
    }
    if (!rxInFrame) continue;

    if (c == ETX) {
      rxBuffer[rxLen] = '\0';
      processFrame(rxBuffer, rxLen);
      rxInFrame = false;
      rxLen = 0;
      continue;
    }
    if (c == '\r') continue;

    if (rxLen < MAX_FRAME_LEN - 1) {
      rxBuffer[rxLen++] = c;
    } else {
      // An over-long frame is line noise; drop it rather than overflowing.
      rxInFrame = false;
      rxLen = 0;
    }
  }
}

static void handleMessage(const String& type, String* args, uint8_t argc) {
  if (type == "PONG") {
    return;  // liveness only
  }

  if (type == "TIME" && argc >= 2) {
    uint32_t unixSec = (uint32_t)strtoul(args[0].c_str(), nullptr, 10);
    int32_t  offset  = (int32_t)strtol(args[1].c_str(), nullptr, 10);
    uint16_t frac    = argc >= 3 ? (uint16_t)strtoul(args[2].c_str(), nullptr, 10) : 0;
    applyTimeSync(unixSec, offset, frac);
    return;
  }

  if (type == "ALARM" && argc >= 2 && args[0] == "ACTIVE") {
    alarmActive = args[1] == "1";
    if (!alarmActive) digitalWrite(LED_ALARM_PIN, LOW);
    return;
  }

  if (type == "DISPLAY" && argc >= 1) {
    if (args[0] == "TEXT" && argc >= 2) {
      displayShowClock = false;
      char text[5] = "    ";
      for (uint8_t i = 0; i < 4 && i < args[1].length(); i++) text[i] = args[1][i];
      text[4] = '\0';
      displaySetText(text);
    } else if (args[0] == "CLOCK") {
      displayShowClock = true;
    } else if (args[0] == "BRIGHTNESS" && argc >= 2) {
      long v = strtol(args[1].c_str(), nullptr, 10);
      if (v < 0) v = 0;
      if (v > 100) v = 100;
      displayBrightness = (uint8_t)v;
    }
    return;
  }

  if (type == "LED" && argc >= 2) {
    setLED(args[0], args[1] == "1");
    return;
  }

  if (type == "CONFIG" && argc >= 2) {
    if (args[0] == "pot_threshold") {
      long v = strtol(args[1].c_str(), nullptr, 10);
      if (v >= 1 && v <= 500) potReportThreshold = (uint16_t)v;
    }
    return;
  }

  // Unknown types are ignored rather than rejected, so a newer Pi can send
  // messages this firmware predates without provoking a stream of NAKs.
}

// ---------------------------------------------------------------------------
// Setup and loop
// ---------------------------------------------------------------------------

void setup() {
  Serial.begin(SERIAL_BAUD);

  displayInit();
  ledsInit();
  potsInit();
  buttonsInit();

  displaySetText("----");

  // Give USB CDC a moment to enumerate, then announce ourselves. The Pi answers
  // HELLO with a full resync, so a reset at any time recovers by itself.
  uint32_t start = millis();
  while (!Serial && (millis() - start) < 3000) {
    displayRefresh();
  }
  sendFrame2("HELLO", FIRMWARE_VERSION, HARDWARE_ID);
}

void loop() {
  // The display is refreshed on every pass. Nothing else in this loop blocks,
  // which is what keeps the multiplexing flicker-free.
  displayRefresh();

  serialPoll();
  potsSample();
  buttonsPoll();

  // Liveness beacon.
  static uint32_t lastPing = 0;
  uint32_t now = millis();
  if (now - lastPing >= PING_INTERVAL_MS) {
    lastPing = now;
    sendFrame1("PING", String(now));
  }

  // If the Pi has gone quiet, keep showing the clock from the local free-running
  // time rather than freezing, and re-announce ourselves so a restarted daemon
  // picks us up promptly.
  static uint32_t lastHello = 0;
  if (lastPiContact != 0 && (now - lastPiContact) > PI_TIMEOUT_MS) {
    if (now - lastHello > PI_TIMEOUT_MS) {
      lastHello = now;
      sendFrame2("HELLO", FIRMWARE_VERSION, HARDWARE_ID);
    }
  }

  if (displayShowClock) displayRenderClock();
  alarmEffects();
}
