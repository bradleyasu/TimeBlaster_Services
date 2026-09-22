#include "Arduino.h"

SerialStub Serial;

namespace hosttest {

uint32_t nowMs = 0;
uint8_t pinState[64] = {0};
int analogValue[64] = {0};

uint8_t chainDataPin = 0;
uint8_t chainClockPin = 0;
uint8_t chainLatchPin = 0;

std::vector<uint8_t> shifted;
std::vector<uint8_t> lastFrame;

// Bit accumulator for the byte currently being clocked in.
static uint8_t partial = 0;
static int partialBits = 0;
// Frame accumulator, flushed on the latch's rising edge.
static std::vector<uint8_t> frameAccum;

void reset() {
  nowMs = 0;
  memset(pinState, 0, sizeof(pinState));
  memset(analogValue, 0, sizeof(analogValue));
  shifted.clear();
  lastFrame.clear();
  frameAccum.clear();
  partial = 0;
  partialBits = 0;
  serialOut.clear();
  serialIn.clear();
  Serial.resetReader();
}

std::string serialOut;
std::string serialIn;

}  // namespace hosttest

// digitalWrite records pin state and, for the shift-register pins, reconstructs
// the bytes being clocked out. A byte is assembled MSB-first on each rising
// clock edge; a rising latch edge closes the frame.
void digitalWrite(uint8_t pin, uint8_t value) {
  using namespace hosttest;

  uint8_t previous = pinState[pin];
  pinState[pin] = value;

  if (chainClockPin != 0 && pin == chainClockPin && previous == LOW && value == HIGH) {
    partial = (uint8_t)((partial << 1) | (pinState[chainDataPin] ? 1 : 0));
    if (++partialBits == 8) {
      shifted.push_back(partial);
      frameAccum.push_back(partial);
      partial = 0;
      partialBits = 0;
    }
    return;
  }

  if (chainLatchPin != 0 && pin == chainLatchPin && previous == LOW && value == HIGH) {
    lastFrame = frameAccum;
    frameAccum.clear();
  }
}
