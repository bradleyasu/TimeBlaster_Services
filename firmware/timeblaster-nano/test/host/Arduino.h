// Minimal Arduino API stub for host-side testing.
//
// It is not an emulator. It provides just enough of the API for the firmware's
// logic to compile and run under clang/gcc, plus two things a real board cannot
// give a test: a settable millis(), and a capture of every bit clocked into the
// shift-register chain. That capture is what lets the display's segment
// patterns, polarity and digit order be asserted without hardware.

#ifndef HOST_ARDUINO_STUB_H
#define HOST_ARDUINO_STUB_H

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

// --- Pin names --------------------------------------------------------------
// The numbers are arbitrary; only their distinctness matters here.
enum : uint8_t {
  D0 = 0, D1, D2, D3, D4, D5, D6, D7, D8, D9, D10, D11, D12, D13,
  A0 = 20, A1, A2, A3, A4, A5, A6, A7,
};

enum : uint8_t { LOW = 0, HIGH = 1 };
enum : uint8_t { INPUT = 0, OUTPUT = 1, INPUT_PULLUP = 2 };
enum : uint8_t { LSBFIRST = 0, MSBFIRST = 1 };

// --- String -----------------------------------------------------------------
// A thin shim over std::string covering the subset the firmware uses.
class String {
 public:
  String() {}
  String(const char* s) : s_(s ? s : "") {}
  String(const std::string& s) : s_(s) {}
  String(char c) : s_(1, c) {}
  String(int v) { char b[24]; snprintf(b, sizeof(b), "%d", v); s_ = b; }
  String(unsigned v) { char b[24]; snprintf(b, sizeof(b), "%u", v); s_ = b; }
  String(long v) { char b[32]; snprintf(b, sizeof(b), "%ld", v); s_ = b; }
  String(unsigned long v) { char b[32]; snprintf(b, sizeof(b), "%lu", v); s_ = b; }

  size_t length() const { return s_.size(); }
  const char* c_str() const { return s_.c_str(); }
  void reserve(size_t n) { s_.reserve(n); }

  char operator[](size_t i) const { return s_[i]; }

  String& operator+=(const String& o) { s_ += o.s_; return *this; }
  String& operator+=(const char* o) { s_ += o; return *this; }
  String& operator+=(char c) { s_ += c; return *this; }

  bool operator==(const char* o) const { return s_ == (o ? o : ""); }
  bool operator==(const String& o) const { return s_ == o.s_; }
  bool operator!=(const char* o) const { return !(*this == o); }

  const std::string& str() const { return s_; }

 private:
  std::string s_;
};

// --- Host test hooks --------------------------------------------------------
namespace hosttest {

// Settable clock.
extern uint32_t nowMs;

// Pin state and history.
extern uint8_t pinState[64];
extern int analogValue[64];

// Which pins carry the shift-register chain. Set by the test to match Pins.h.
extern uint8_t chainDataPin;
extern uint8_t chainClockPin;
extern uint8_t chainLatchPin;

// Bytes reconstructed from the chain, in the order they were clocked out.
extern std::vector<uint8_t> shifted;

// Bytes captured between the most recent pair of latch edges, i.e. one frame.
extern std::vector<uint8_t> lastFrame;

// Serial capture and injection.
extern std::string serialOut;
extern std::string serialIn;

void reset();

}  // namespace hosttest

// --- Digital and analog I/O -------------------------------------------------
inline void pinMode(uint8_t, uint8_t) {}

void digitalWrite(uint8_t pin, uint8_t value);

inline int digitalRead(uint8_t pin) { return hosttest::pinState[pin]; }
inline int analogRead(uint8_t pin) { return hosttest::analogValue[pin]; }
inline void analogReadResolution(int) {}
inline void analogWrite(uint8_t pin, int value) { hosttest::analogValue[pin] = value; }

inline uint32_t millis() { return hosttest::nowMs; }
inline void delayMicroseconds(unsigned) {}
inline void delay(unsigned ms) { hosttest::nowMs += ms; }

// --- Serial -----------------------------------------------------------------
class SerialStub {
 public:
  void begin(unsigned long) {}
  explicit operator bool() const { return true; }

  int available() const { return (int)(hosttest::serialIn.size() - readPos_); }
  int read() {
    if (readPos_ >= hosttest::serialIn.size()) return -1;
    return (unsigned char)hosttest::serialIn[readPos_++];
  }
  void resetReader() { readPos_ = 0; }

  void write(char c) { hosttest::serialOut.push_back(c); }
  void write(uint8_t c) { hosttest::serialOut.push_back((char)c); }
  void print(const String& s) { hosttest::serialOut += s.str(); }
  void print(const char* s) { hosttest::serialOut += s; }

 private:
  size_t readPos_ = 0;
};

extern SerialStub Serial;

#endif  // HOST_ARDUINO_STUB_H
