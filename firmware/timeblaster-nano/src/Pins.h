#ifndef TIMEBLASTER_PINS_H
#define TIMEBLASTER_PINS_H

#include <Arduino.h>

// Every pin assignment for the Timeblaster, in one place.
//
// The display pins are not negotiable: they are fixed by the PCB (see
// pcb/timeblaster in the TimeblasterClock repository). Everything else was
// chosen to work around them.

// --- 7-segment display ------------------------------------------------------
//
// Four 74HC595 shift registers in a daisy chain, one per digit, driving two
// HDSP-K511 dual-digit common-anode displays through 32 x 360R resistors.
//
// These three constants are documentation only: SevenSegment.cpp declares its
// own copies, and that file is carried over from the working driver unmodified
// so it stays diffable against the original. If the wiring ever changes, change
// it there and update these to match.
static const uint8_t DISPLAY_DATA_PIN  = D5;
static const uint8_t DISPLAY_CLOCK_PIN = D6;
static const uint8_t DISPLAY_LATCH_PIN = D7;

// The 595s' ~OE is tied low on the board and is not brought out on J2, which is
// a 5-pin header carrying only 3V3, GND, DATA, CLOCK and LATCH. There is
// therefore no hardware dimming: see Display.cpp for what BRIGHTNESS does
// instead, and wire ~OE to a PWM-capable pin here if real dimming is ever
// wanted.
static const int DISPLAY_OE_PIN = -1;   // -1 means "not wired"

// How many digits are on the chain.
static const int DISPLAY_DIGIT_COUNT = 4;

// --- Potentiometers ---------------------------------------------------------
//
// 10k linear, each wired 3V3 / wiper / GND. 3V3 only: the ESP32-S3's ADC inputs
// are not 5V tolerant.
//
// Which knob means what is decided entirely on the Raspberry Pi. The firmware
// only knows there are four of them.
static const uint8_t POT_PINS[] = {A0, A1, A2, A3};
static const uint8_t POT_COUNT  = sizeof(POT_PINS) / sizeof(POT_PINS[0]);

// --- Buttons ----------------------------------------------------------------
//
// Wired pin -> GND, using the internal pull-up, so they read LOW when pressed.
// No external resistor needed.
static const uint8_t BUTTON_WIFI_PIN      = D2;
static const uint8_t BUTTON_ALARM_OFF_PIN = D3;

// --- LEDs -------------------------------------------------------------------
//
// Each: pin -> 220R -> LED anode, cathode -> GND.
//
// D5 and D6 would have been the obvious next pins along, but they belong to the
// display's DATA and CLOCK lines, so the Wi-Fi and power LEDs sit above the
// display's block instead.
static const uint8_t LED_ALARM_PIN = D4;
static const uint8_t LED_WIFI_PIN  = D8;
static const uint8_t LED_POWER_PIN = D9;

#endif // TIMEBLASTER_PINS_H
