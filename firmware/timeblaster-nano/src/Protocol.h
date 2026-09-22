#ifndef TIMEBLASTER_PROTOCOL_H
#define TIMEBLASTER_PROTOCOL_H

#include <Arduino.h>

// The TB1 wire format spoken with the Raspberry Pi over USB CDC serial.
//
//   <STX> TB1 | SEQ | TYPE | arg | arg ... | CRC <LF>
//    0x02  ^------------ checksummed region ------^   0x0A
//
// The same format is implemented in Go in internal/protocol, and that package's
// tests pin the exact bytes and cross-check this CRC against a transcription of
// the implementation below. A change on either side that breaks compatibility
// fails a test rather than a bench session.
//
// See docs/serial-protocol.md for the full specification.

static const char PROTOCOL_STX = 0x02;
static const char PROTOCOL_ETX = '\n';
static const uint16_t PROTOCOL_MAX_FRAME_LEN = 512;
static const uint16_t PROTOCOL_MAX_SEQ = 4096;
static const uint8_t PROTOCOL_MAX_ARGS = 6;

// A decoded inbound frame.
struct Message {
  String type;
  String args[PROTOCOL_MAX_ARGS];
  uint8_t argc;
};

// CRC-8, polynomial 0x07, init 0x00.
uint8_t protocolCRC8(const char* data, size_t len);

// Sending. Each returns after the frame has been written to Serial.
void protocolSend(const String& type, const String* args, uint8_t argc);
void protocolSend0(const String& type);
void protocolSend1(const String& type, const String& a);
void protocolSend2(const String& type, const String& a, const String& b);

// protocolPoll reads available bytes and invokes handler for each complete,
// checksum-valid frame. Anything before an STX is discarded, which is how the
// link resynchronises after a reset or a half-transmitted frame.
void protocolPoll(void (*handler)(const Message&));

#endif // TIMEBLASTER_PROTOCOL_H
