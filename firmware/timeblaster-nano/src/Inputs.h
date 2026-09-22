#ifndef TIMEBLASTER_INPUTS_H
#define TIMEBLASTER_INPUTS_H

#include <Arduino.h>

// Potentiometers, buttons and LEDs.
//
// This layer reports what it observes and renders what it is told. It holds no
// application policy: it does not know that pot 0 selects channels, that a
// five-second hold means Wi-Fi setup, or what an alarm is. All of that lives on
// the Raspberry Pi, where it is configurable and unit-tested.
//
// What it does own is the work that benefits from being close to the hardware:
// oversampling and smoothing the noisy ESP32 ADC, debouncing the buttons, and
// rate-limiting reports so the serial link is not flooded with ADC noise.

void inputsInit();

// inputsPoll samples the pots and buttons, emitting POT and BUTTON frames as
// they change. Call it every pass of loop().
void inputsPoll();

// inputsSetPotThreshold changes how much a filtered reading must move before it
// is reported, in raw ADC counts. Driven by the Pi's CONFIG message.
void inputsSetPotThreshold(uint16_t counts);

// LEDs are addressed by logical name, never by pin number, so rewiring never
// requires a change on the Pi.
void ledsSet(const String& name, bool on);

// ledsSetAlarmActive starts or stops the alarm LED blink.
void ledsSetAlarmActive(bool active);

// ledsTick advances the alarm blink. Call it every pass of loop().
void ledsTick();

#endif // TIMEBLASTER_INPUTS_H
