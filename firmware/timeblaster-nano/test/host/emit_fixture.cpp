// Emits a capture of the firmware's encoder for the Go interop tests to decode.
//
// The messages here are the ones worth pinning: the handshake, a liveness
// beacon, both ends of the potentiometer range, both button edges, and an
// argument containing every character that would break framing if escaping were
// wrong on either side.
//
// Regenerate with: make -C firmware/timeblaster-nano fixture
// The expectations live in internal/protocol/interop_test.go and must be kept
// in step with this list.

#include "Arduino.h"
#include "../../src/Protocol.h"

#include <cstdio>

int main() {
  hosttest::reset();

  protocolSend2("HELLO", "1.0.0", "nano-esp32");
  protocolSend1("PING", "12345");
  protocolSend2("POT", "0", "742");
  protocolSend2("POT", "3", "4095");
  protocolSend2("BUTTON", "WIFI", "DOWN");
  protocolSend2("BUTTON", "ALARM_OFF", "UP");
  protocolSend2("LOG", "warn", "a|b\\c\nd");

  fwrite(hosttest::serialOut.data(), 1, hosttest::serialOut.size(), stdout);
  return 0;
}
