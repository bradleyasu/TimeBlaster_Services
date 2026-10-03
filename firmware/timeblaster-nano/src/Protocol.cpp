#include "Protocol.h"

static uint16_t txSeq = 0;
// Frames abandoned because the USB CDC buffer was full. Kept so a wedged host
// is visible rather than silently lossy, and reported in every PING so the Pi
// can see it without anyone standing at the device.
static uint32_t txDropped = 0;

// When a frame was last actually written. Zero until the first send.
//
// This is what makes a dead transmit path detectable from inside the firmware.
// The Nano cannot be told that nothing is arriving -- that message would travel
// the broken direction -- but it can notice that it is still being spoken to
// while nothing it sends gets out.
static uint32_t txLastOkMs = 0;

uint8_t protocolCRC8(const char* data, size_t len) {
  uint8_t crc = 0;
  for (size_t i = 0; i < len; i++) {
    crc ^= (uint8_t)data[i];
    for (uint8_t b = 0; b < 8; b++) {
      crc = (crc & 0x80) ? (uint8_t)((crc << 1) ^ 0x07) : (uint8_t)(crc << 1);
    }
  }
  return crc;
}

// appendEscaped writes s into out, escaping the bytes that would break framing
// so that display text can contain anything.
static void appendEscaped(String& out, const String& s) {
  for (size_t i = 0; i < s.length(); i++) {
    char c = s[i];
    switch (c) {
      case '\\':         out += "\\\\"; break;
      case '|':          out += "\\p";  break;
      case '\n':         out += "\\n";  break;
      case '\r':         out += "\\r";  break;
      case PROTOCOL_STX: out += "\\s";  break;
      default:           out += c;      break;
    }
  }
}

void protocolSend(const String& type, const String* args, uint8_t argc) {
  String body = "TB1|";
  body += String(txSeq % PROTOCOL_MAX_SEQ);
  body += '|';
  body += type;
  for (uint8_t i = 0; i < argc; i++) {
    body += '|';
    appendEscaped(body, args[i]);
  }
  txSeq = (txSeq + 1) % PROTOCOL_MAX_SEQ;

  char crcHex[3];
  snprintf(crcHex, sizeof(crcHex), "%02X", protocolCRC8(body.c_str(), body.length()));

  // Frame bytes: STX + body + '|' + two CRC digits + ETX.
  const int frameLen = (int)body.length() + 5;

  // Never start a frame that will not fit. On the Nano ESP32 Serial is USB CDC,
  // and a write with no room blocks in the USB stack until the host drains the
  // buffer. Blocking here stalls loop(), which stops the PINGs, the display and
  // the input polling -- the whole firmware waiting on a host that may never
  // read again. Dropping the frame is harmless by comparison: PING repeats every
  // second, and input reports are resent on the next change.
  if (!Serial || Serial.availableForWrite() < frameLen) {
    txDropped++;
    return;
  }

  Serial.write(PROTOCOL_STX);
  Serial.print(body);
  Serial.write('|');
  Serial.print(crcHex);
  Serial.write(PROTOCOL_ETX);
  txLastOkMs = millis();
}

uint32_t protocolDroppedFrames() { return txDropped; }

uint32_t protocolLastTxOkMs() { return txLastOkMs; }

bool protocolTxStalled(uint32_t now, uint32_t lastRxMs,
                       uint32_t txStallMs, uint32_t rxFreshMs) {
  // The Pi has to have been heard from recently. Both directions idle is an
  // absent or restarting daemon, not a broken transmitter, and restarting over
  // that would turn every deploy into a reboot.
  //
  // This also covers a Nano that has never heard the Pi at all: lastRxMs is zero
  // until the first message, so the gap is the whole uptime and far past any
  // sensible freshness window. An explicit zero check sat here first and no test
  // could reach it.
  if (now - lastRxMs > rxFreshMs) return false;

  // Still being spoken to, but nothing has left here in a long time.
  return now - txLastOkMs > txStallMs;
}

void protocolResetTxHealth(uint32_t now) {
  txLastOkMs = now;
  txDropped = 0;
}

void protocolSend0(const String& type) { protocolSend(type, nullptr, 0); }

void protocolSend1(const String& type, const String& a) {
  String args[1] = {a};
  protocolSend(type, args, 1);
}

void protocolSend2(const String& type, const String& a, const String& b) {
  String args[2] = {a, b};
  protocolSend(type, args, 2);
}

// unescapeField reverses appendEscaped.
static String unescapeField(const char* s, uint16_t len) {
  String out;
  out.reserve(len);
  for (uint16_t i = 0; i < len; i++) {
    if (s[i] == '\\' && i + 1 < len) {
      i++;
      switch (s[i]) {
        case '\\': out += '\\';         break;
        case 'p':  out += '|';          break;
        case 'n':  out += '\n';         break;
        case 'r':  out += '\r';         break;
        case 's':  out += PROTOCOL_STX; break;
        default:   out += s[i];         break;
      }
    } else {
      out += s[i];
    }
  }
  return out;
}

// processFrame validates one frame body and hands it to the caller's handler.
static void processFrame(char* body, uint16_t len, void (*handler)(const Message&)) {
  if (len == 0) {
    return;
  }

  // Split on unescaped separators.
  static const uint8_t MAX_FIELDS = PROTOCOL_MAX_ARGS + 4;
  char* starts[MAX_FIELDS];
  uint16_t lens[MAX_FIELDS];
  uint8_t count = 0;

  uint16_t fieldStart = 0;
  for (uint16_t i = 0; i <= len && count < MAX_FIELDS; i++) {
    if (i < len && body[i] == '\\') {
      i++;
      continue;
    }
    if (i == len || body[i] == '|') {
      starts[count] = body + fieldStart;
      lens[count] = i - fieldStart;
      count++;
      fieldStart = i + 1;
    }
  }
  if (count < 4) {   // version | seq | type | crc
    return;
  }

  if (lens[0] != 3 || strncmp(starts[0], "TB1", 3) != 0) {
    return;   // unknown protocol version
  }

  // The checksum covers everything before the separator preceding the CRC.
  uint16_t covered = len - lens[count - 1] - 1;
  char expected[3];
  snprintf(expected, sizeof(expected), "%02X", protocolCRC8(body, covered));
  if (lens[count - 1] != 2 || strncasecmp(starts[count - 1], expected, 2) != 0) {
    return;   // corrupt or truncated
  }

  Message msg;
  msg.type = unescapeField(starts[2], lens[2]);
  msg.argc = 0;
  for (uint8_t i = 3; i < count - 1 && msg.argc < PROTOCOL_MAX_ARGS; i++) {
    msg.args[msg.argc++] = unescapeField(starts[i], lens[i]);
  }

  handler(msg);
}

void protocolPoll(void (*handler)(const Message&)) {
  static char rxBuffer[PROTOCOL_MAX_FRAME_LEN];
  static uint16_t rxLen = 0;
  static bool inFrame = false;

  while (Serial.available() > 0) {
    char c = (char)Serial.read();

    if (c == PROTOCOL_STX) {
      inFrame = true;
      rxLen = 0;
      continue;
    }
    if (!inFrame) {
      continue;   // resynchronising
    }

    if (c == PROTOCOL_ETX) {
      rxBuffer[rxLen] = '\0';
      processFrame(rxBuffer, rxLen, handler);
      inFrame = false;
      rxLen = 0;
      continue;
    }
    if (c == '\r') {
      continue;   // tolerate CRLF
    }

    if (rxLen < PROTOCOL_MAX_FRAME_LEN - 1) {
      rxBuffer[rxLen++] = c;
    } else {
      // An over-long frame is line noise; drop it rather than overflowing.
      inFrame = false;
      rxLen = 0;
    }
  }
}
