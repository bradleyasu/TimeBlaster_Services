#include "Inputs.h"
#include "Pins.h"
#include "Protocol.h"

// --- Potentiometers ---------------------------------------------------------
//
// The ESP32-S3 ADC is 12-bit and noticeably noisy, so each reported value is
// the mean of several samples passed through an exponential moving average.
// This is smoothing, not policy: the Pi still does calibration, dead zones,
// hysteresis and range mapping.

static const uint8_t  POT_OVERSAMPLE = 16;
static const float    POT_EMA_ALPHA  = 0.25f;
static const uint16_t POT_SAMPLE_MS  = 20;

// A reading is only sent when it has moved by at least this much, or when
// POT_HEARTBEAT_MS has elapsed. The default is well below anything a human hand
// produces, so deliberate movement is never missed while ADC noise is not
// allowed to flood the link.
static uint16_t potReportThreshold = 12;
static const uint32_t POT_HEARTBEAT_MS = 5000;

static float    potFiltered[POT_COUNT];
static uint16_t potLastSent[POT_COUNT];
static uint32_t potLastSentAt[POT_COUNT];
static bool     potPrimed[POT_COUNT];

// --- Buttons ----------------------------------------------------------------

static const uint16_t DEBOUNCE_MS = 25;

struct Button {
  uint8_t     pin;
  const char* name;
  bool        stable;        // debounced state: true when pressed
  bool        lastReading;
  uint32_t    lastChange;
};

static Button buttons[] = {
  {BUTTON_WIFI_PIN,      "WIFI",      false, false, 0},
  {BUTTON_ALARM_OFF_PIN, "ALARM_OFF", false, false, 0},
};
static const uint8_t BUTTON_COUNT = sizeof(buttons) / sizeof(buttons[0]);

// --- LEDs -------------------------------------------------------------------

static const uint32_t ALARM_BLINK_MS = 400;   // matches the display flash
static bool alarmActive = false;

void inputsInit() {
  analogReadResolution(12);
  for (uint8_t i = 0; i < POT_COUNT; i++) {
    potFiltered[i]   = 0;
    potLastSent[i]   = 0;
    potLastSentAt[i] = 0;
    potPrimed[i]     = false;
  }

  for (uint8_t i = 0; i < BUTTON_COUNT; i++) {
    pinMode(buttons[i].pin, INPUT_PULLUP);
    buttons[i].stable      = false;
    buttons[i].lastReading = false;
    buttons[i].lastChange  = 0;
  }

  pinMode(LED_ALARM_PIN, OUTPUT);
  pinMode(LED_WIFI_PIN, OUTPUT);
  pinMode(LED_POWER_PIN, OUTPUT);
  digitalWrite(LED_ALARM_PIN, LOW);
  digitalWrite(LED_WIFI_PIN, LOW);
  digitalWrite(LED_POWER_PIN, HIGH);
}

void inputsSetPotThreshold(uint16_t counts) {
  if (counts >= 1 && counts <= 500) {
    potReportThreshold = counts;
  }
}

static void potsPoll() {
  static uint32_t lastSample = 0;
  uint32_t now = millis();
  if (now - lastSample < POT_SAMPLE_MS) {
    return;
  }
  lastSample = now;

  for (uint8_t i = 0; i < POT_COUNT; i++) {
    uint32_t sum = 0;
    for (uint8_t s = 0; s < POT_OVERSAMPLE; s++) {
      sum += analogRead(POT_PINS[i]);
    }
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
    int32_t delta = (int32_t)value - (int32_t)potLastSent[i];
    if (delta < 0) {
      delta = -delta;
    }

    bool moved     = (uint16_t)delta >= potReportThreshold;
    bool overdue   = (now - potLastSentAt[i]) >= POT_HEARTBEAT_MS;
    bool firstEver = potLastSentAt[i] == 0;

    if (moved || overdue || firstEver) {
      protocolSend2("POT", String(i), String(value));
      potLastSent[i]   = value;
      potLastSentAt[i] = now;
    }
  }
}

// buttonsPoll debounces and reports edges only. The five-second hold that
// enters Wi-Fi setup mode is measured on the Pi, where its duration is
// configurable and its behaviour is unit-tested.
static void buttonsPoll() {
  uint32_t now = millis();
  for (uint8_t i = 0; i < BUTTON_COUNT; i++) {
    bool reading = digitalRead(buttons[i].pin) == LOW;   // active low

    if (reading != buttons[i].lastReading) {
      buttons[i].lastReading = reading;
      buttons[i].lastChange  = now;
      continue;
    }
    if (reading != buttons[i].stable && (now - buttons[i].lastChange) >= DEBOUNCE_MS) {
      buttons[i].stable = reading;
      protocolSend2("BUTTON", String(buttons[i].name), reading ? "DOWN" : "UP");
    }
  }
}

void inputsPoll() {
  potsPoll();
  buttonsPoll();
}

void ledsSet(const String& name, bool on) {
  if (name == "ALARM") {
    // An explicit LED command wins over the blink until the next alarm state
    // change, so the Pi can always force it off.
    digitalWrite(LED_ALARM_PIN, on ? HIGH : LOW);
  } else if (name == "WIFI") {
    digitalWrite(LED_WIFI_PIN, on ? HIGH : LOW);
  } else if (name == "POWER") {
    digitalWrite(LED_POWER_PIN, on ? HIGH : LOW);
  }
}

void ledsSetAlarmActive(bool active) {
  alarmActive = active;
  if (!active) {
    digitalWrite(LED_ALARM_PIN, LOW);
  }
}

void ledsTick() {
  if (!alarmActive) {
    return;
  }
  bool phase = (millis() / ALARM_BLINK_MS) % 2 == 0;
  digitalWrite(LED_ALARM_PIN, phase ? HIGH : LOW);
}
