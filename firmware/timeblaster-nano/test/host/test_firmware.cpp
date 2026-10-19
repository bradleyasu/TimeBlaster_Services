// Host-side tests for the Timeblaster firmware.
//
// These run under clang/gcc with the Arduino API stubbed, so the display's
// segment patterns, polarity and digit order can be asserted without a board —
// and so a regression in any of them fails here rather than on the bench.
//
//   make -C firmware/timeblaster-nano test
//
// What they deliberately do NOT cover: the real 74HC595 timing, the real ADC,
// and anything about whether the physical wiring matches Pins.h.

#include "Arduino.h"

#include "../../src/Display.h"
#include "../../src/Inputs.h"
#include "../../src/Pins.h"
#include "../../src/Protocol.h"
#include "../../src/SevenSegment.h"
#include "../../src/Shutdown.h"

#include <cstdio>
#include <string>
#include <vector>

static int failures = 0;
static int checks = 0;

static void check(bool ok, const char* what) {
  checks++;
  if (!ok) {
    failures++;
    printf("  FAIL  %s\n", what);
  }
}

static void checkBytes(const std::vector<uint8_t>& got,
                       const std::vector<uint8_t>& want,
                       const char* what) {
  checks++;
  if (got == want) return;
  failures++;
  printf("  FAIL  %s\n        got: ", what);
  for (uint8_t b : got) printf("0x%02X ", b);
  printf("\n       want: ");
  for (uint8_t b : want) printf("0x%02X ", b);
  printf("\n");
}

static void section(const char* name) { printf("\n%s\n", name); }

static void setupChainCapture() {
  hosttest::reset();
  hosttest::chainDataPin  = DISPLAY_DATA_PIN;
  hosttest::chainClockPin = DISPLAY_CLOCK_PIN;
  hosttest::chainLatchPin = DISPLAY_LATCH_PIN;
}

// --- The character table ----------------------------------------------------
//
// The segment bit order is the thing most likely to be got wrong, and it is
// invisible until a display is wired up. Pin the digits against the segment
// constants the driver declares.

static void testCharacterTable() {
  section("character table");

  // Each digit, spelled out from the named segments rather than from a literal,
  // so the assertion says what shape it expects.
  struct {
    char c;
    uint8_t want;
    const char* desc;
  } cases[] = {
    {'0', (uint8_t)(SEG_A | SEG_B | SEG_C | SEG_D | SEG_E | SEG_F),         "0 = all but middle"},
    {'1', (uint8_t)(SEG_B | SEG_C),                                          "1 = two right segments"},
    {'2', (uint8_t)(SEG_A | SEG_B | SEG_G | SEG_E | SEG_D),                  "2"},
    {'3', (uint8_t)(SEG_A | SEG_B | SEG_G | SEG_C | SEG_D),                  "3"},
    {'4', (uint8_t)(SEG_F | SEG_G | SEG_B | SEG_C),                          "4"},
    {'5', (uint8_t)(SEG_A | SEG_F | SEG_G | SEG_C | SEG_D),                  "5"},
    {'6', (uint8_t)(SEG_A | SEG_F | SEG_G | SEG_E | SEG_C | SEG_D),          "6"},
    {'7', (uint8_t)(SEG_A | SEG_B | SEG_C),                                  "7"},
    {'8', (uint8_t)(SEG_A | SEG_B | SEG_C | SEG_D | SEG_E | SEG_F | SEG_G),  "8 = every segment"},
    // This driver draws 9 without its bottom segment. Both renderings are
    // common; this one is what the working hardware shows.
    {'9', (uint8_t)(SEG_A | SEG_B | SEG_C | SEG_F | SEG_G),                  "9 (no tail)"},
    {' ', 0,                                                                 "space = blank"},
    {'-', SEG_G,                                                             "- = middle only"},
  };

  for (auto& tc : cases) {
    uint8_t got = getCharacter(tc.c);
    checks++;
    if (got != tc.want) {
      failures++;
      printf("  FAIL  %s: getCharacter('%c') = 0x%02X, want 0x%02X\n",
             tc.desc, tc.c, got, tc.want);
    }
  }

  // The letters the Pi actually sends as status words must be legible.
  check(getCharacter('S') != 0, "S renders");
  check(getCharacter('E') != 0, "E renders");
  check(getCharacter('t') != 0, "t renders");
  check(getCharacter('U') != 0, "U renders");
  check(getCharacter('P') != 0, "P renders");
  check(getCharacter('r') != 0, "r renders");

  // An unmapped character is blank rather than garbage.
  check(getCharacter('\x01') == 0, "unknown character is blank");
}

// --- Wire order, polarity and digit order -----------------------------------

static void testFramePolarityAndOrder() {
  section("shift-register frame");

  setupChainCapture();
  initializeSevenSegmentDisplay(DISPLAY_DIGIT_COUNT);

  // "12.30" is four cells: '1', '2' with its decimal point lit, '3', '0'.
  setDisplayText("12.30");
  paintDisplay();

  // Segments are active low on this common-anode hardware, so the driver
  // inverts them; the decimal point is bit 0 and is likewise cleared to light.
  const uint8_t DOT = 0b00000001;
  uint8_t cell0 = (uint8_t)(~getCharacter('1') | DOT);            // no dot
  uint8_t cell1 = (uint8_t)(~getCharacter('2') & (uint8_t)~DOT);  // dot lit
  uint8_t cell2 = (uint8_t)(~getCharacter('3') | DOT);
  uint8_t cell3 = (uint8_t)(~getCharacter('0') | DOT);

  // The byte clocked out first travels furthest down the chain and lands on the
  // rightmost digit, so the cells go out back to front.
  checkBytes(hosttest::lastFrame, {cell3, cell2, cell1, cell0},
             "frame is inverted, dotted and ordered right-to-left");

  // And the concrete bytes, so a change to any of the three rules above is
  // visible as a number rather than as an expression that changed with it.
  checkBytes(hosttest::lastFrame, {0x11, 0x23, 0x28, 0xB7},
             "frame bytes for \"12.30\"");

  check(hosttest::lastFrame.size() == 4, "one byte per digit");
}

static void testBlankAndFullFrames() {
  section("blank and full frames");

  setupChainCapture();
  initializeSevenSegmentDisplay(4);

  setDisplayText("    ");
  paintDisplay();
  // Blank: every segment off, so every bit high on active-low hardware.
  checkBytes(hosttest::lastFrame, {0xFF, 0xFF, 0xFF, 0xFF}, "all blank is 0xFF x4");

  setDisplayText("8.8.8.8.");
  paintDisplay();
  // Every segment and every point lit, so every bit low.
  checkBytes(hosttest::lastFrame, {0x00, 0x00, 0x00, 0x00}, "all lit is 0x00 x4");
}

static void testDigitCountClamping() {
  section("digit count");

  setupChainCapture();
  initializeSevenSegmentDisplay(2);
  check(getDisplayDigitCount() == 2, "two digits honoured");
  setDisplayText("12");
  paintDisplay();
  check(hosttest::lastFrame.size() == 2, "frame sized to the chain");

  initializeSevenSegmentDisplay(99);
  check(getDisplayDigitCount() == 8, "oversized count clamped to 8");
  initializeSevenSegmentDisplay(0);
  check(getDisplayDigitCount() == 1, "zero clamped to 1");

  initializeSevenSegmentDisplay(DISPLAY_DIGIT_COUNT);
}

static void testScrolling() {
  section("scrolling");

  setupChainCapture();
  initializeSevenSegmentDisplay(4);

  setDisplayText("1234");
  check(!isDisplayScrolling(), "content that fits does not scroll");

  setDisplayText("SETUP");
  check(isDisplayScrolling(), "content longer than the display scrolls");

  setDisplayText("12.34");
  check(!isDisplayScrolling(),
        "a decimal point binds to the digit before it rather than taking a cell");
}

// --- The Timeblaster display layer ------------------------------------------

static void testClockRendering() {
  section("clock rendering");

  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetBrightness(75);
  displayShowClock();

  // 9:30 with the colon lit. A leading space, not a leading zero.
  hosttest::nowMs = 0;   // first half of the second, so the point is on
  displaySetTime(9, 30);
  displayTick();

  const uint8_t DOT = 0b00000001;
  uint8_t blank = (uint8_t)(~getCharacter(' ') | DOT);
  uint8_t nine  = (uint8_t)(~getCharacter('9') & (uint8_t)~DOT);   // colon
  uint8_t three = (uint8_t)(~getCharacter('3') | DOT);
  uint8_t zero  = (uint8_t)(~getCharacter('0') | DOT);
  checkBytes(hosttest::lastFrame, {zero, three, nine, blank},
             "9:30 renders as \" 9.30\" with a leading blank");

  // Half a second later the colon is dark.
  hosttest::nowMs = 600;
  displayTick();
  uint8_t nineNoDot = (uint8_t)(~getCharacter('9') | DOT);
  checkBytes(hosttest::lastFrame, {zero, three, nineNoDot, blank},
             "the colon blinks off on the half second");

  // Noon is 12, not 0.
  hosttest::nowMs = 1000;
  displaySetTime(12, 5);
  displayTick();
  uint8_t one  = (uint8_t)(~getCharacter('1') | DOT);
  uint8_t two  = (uint8_t)(~getCharacter('2') & (uint8_t)~DOT);
  uint8_t five = (uint8_t)(~getCharacter('5') | DOT);
  checkBytes(hosttest::lastFrame, {five, zero, two, one}, "12:05 renders as \"12.05\"");
}

static void testAlarmPendingDot() {
  section("alarm pending dot");

  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetBrightness(75);
  displayShowClock();

  const uint8_t DOT = 0b00000001;
  // Segments are active low and so is the point: clearing the bit lights it.
  auto lit   = [&](char c) { return (uint8_t)(~getCharacter(c) & (uint8_t)~DOT); };
  auto dark  = [&](char c) { return (uint8_t)(~getCharacter(c) | DOT); };

  // 10:40 in the second half of the second, so the colon point is dark and the
  // only point that can be lit is the one under test.
  hosttest::nowMs = 600;
  displaySetTime(22, 40);
  displaySetAlarmPending(false);
  displayTick();
  checkBytes(hosttest::lastFrame, {dark('0'), dark('4'), dark('0'), dark('1')},
             "no alarm within the day leaves every point dark");

  // The same minute with an alarm due: "10.40." -- the trailing point binds to
  // the last digit rather than taking a cell, so no digit is lost.
  displaySetAlarmPending(true);
  displayTick();
  checkBytes(hosttest::lastFrame, {lit('0'), dark('4'), dark('0'), dark('1')},
             "an alarm within the day lights the last digit's point");

  // It must not blink with the colon. Half a second earlier the colon is lit
  // and the alarm point has to stay lit too.
  hosttest::nowMs = 100;
  displayTick();
  checkBytes(hosttest::lastFrame, {lit('0'), dark('4'), lit('0'), dark('1')},
             "the alarm point stays lit while the colon blinks on");

  // And clearing it puts the display back.
  displaySetAlarmPending(false);
  hosttest::nowMs = 600;
  displayTick();
  checkBytes(hosttest::lastFrame, {dark('0'), dark('4'), dark('0'), dark('1')},
             "clearing the alarm puts the point out");
}

static void testClock24h() {
  section("24-hour clock");

  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetBrightness(75);
  displayShowClock();
  hosttest::nowMs = 0;   // colon lit

  const uint8_t DOT = 0b00000001;
  auto cell = [&](char c, bool colon) {
    return colon ? (uint8_t)(~getCharacter(c) & (uint8_t)~DOT)
                 : (uint8_t)(~getCharacter(c) | DOT);
  };

  check(!displayClock24h(), "12-hour is the default");

  // The afternoon is where the two modes actually differ.
  displaySetTime(21, 30);
  displayTick();
  checkBytes(hosttest::lastFrame,
             {cell('0', false), cell('3', false), cell('9', true), cell(' ', false)},
             "21:30 shows as \" 9.30\" in 12-hour mode");

  displaySetClock24h(true);
  check(displayClock24h(), "the mode is reported back");
  displayTick();
  checkBytes(hosttest::lastFrame,
             {cell('0', false), cell('3', false), cell('1', true), cell('2', false)},
             "21:30 shows as \"21.30\" in 24-hour mode");

  // Morning hours keep their leading zero in 24-hour form, which is both the
  // convention and the cue for which mode the clock is in.
  displaySetTime(9, 5);
  displayTick();
  checkBytes(hosttest::lastFrame,
             {cell('5', false), cell('0', false), cell('9', true), cell('0', false)},
             "09:05 keeps its leading zero in 24-hour mode");

  // Midnight is the case the old code could not express: hour 0 doubled as the
  // "no time yet" sentinel, so 00:xx would have rendered as four dashes.
  displaySetTime(0, 7);
  displayTick();
  checkBytes(hosttest::lastFrame,
             {cell('7', false), cell('0', false), cell('0', true), cell('0', false)},
             "midnight renders as \"00.07\", not as the no-time dashes");

  displaySetClock24h(false);
  displayTick();
  checkBytes(hosttest::lastFrame,
             {cell('7', false), cell('0', false), cell('2', true), cell('1', false)},
             "midnight is 12 in 12-hour mode");

  // And with no time at all it is still dashes.
  displayInit();
  displaySetSynced(true);
  displayShowClock();
  displayTick();
  uint8_t dash = (uint8_t)(~getCharacter('-') | DOT);
  checkBytes(hosttest::lastFrame, {dash, dash, dash, dash},
             "no time received yet still shows dashes");
}

static void testOverrideTextAndReturn() {
  section("override text");

  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetBrightness(75);
  displaySetTime(6, 30);

  displaySetText("SETUP");
  displayTick();
  check(isDisplayScrolling(), "SETUP scrolls across four digits");

  displayShowClock();
  hosttest::nowMs = 0;
  displayTick();
  check(!isDisplayScrolling(), "returning to the clock stops the scroll");

  const uint8_t DOT = 0b00000001;
  uint8_t blank = (uint8_t)(~getCharacter(' ') | DOT);
  uint8_t six   = (uint8_t)(~getCharacter('6') & (uint8_t)~DOT);
  uint8_t three = (uint8_t)(~getCharacter('3') | DOT);
  uint8_t zero  = (uint8_t)(~getCharacter('0') | DOT);
  checkBytes(hosttest::lastFrame, {zero, three, six, blank}, "the time is back");
}

static void testBrightnessZeroBlanks() {
  section("brightness");

  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetTime(6, 30);

  displaySetBrightness(0);
  displayTick();
  checkBytes(hosttest::lastFrame, {0xFF, 0xFF, 0xFF, 0xFF},
             "brightness 0 blanks the display");

  displaySetBrightness(100);
  hosttest::nowMs = 0;
  displayTick();
  check(hosttest::lastFrame != std::vector<uint8_t>({0xFF, 0xFF, 0xFF, 0xFF}),
        "brightness above 0 restores the display");
}

static void testAlarmFlash() {
  section("alarm flash");

  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetBrightness(75);
  displaySetTime(6, 30);
  displaySetAlarmActive(true);

  // The flash alternates on a 400 ms period.
  hosttest::nowMs = 0;
  displayTick();
  bool litAtZero = hosttest::lastFrame != std::vector<uint8_t>({0xFF, 0xFF, 0xFF, 0xFF});

  hosttest::nowMs = 400;
  displayTick();
  bool blankAt400 = hosttest::lastFrame == std::vector<uint8_t>({0xFF, 0xFF, 0xFF, 0xFF});

  check(litAtZero && blankAt400, "an active alarm flashes the display");

  displaySetAlarmActive(false);
  hosttest::nowMs = 400;
  displayTick();
  check(hosttest::lastFrame != std::vector<uint8_t>({0xFF, 0xFF, 0xFF, 0xFF}),
        "clearing the alarm stops the flash");
}

static void testLoadingUntilSynced() {
  section("waiting for the Pi");

  setupChainCapture();
  displayInit();
  check(isLoading(), "the loading animation runs before the first time sync");

  displaySetSynced(true);
  check(!isLoading(), "a time sync hands the display back to the clock");

  displaySetSynced(false);
  check(isLoading(), "losing sync resumes the animation");

  // Turning the display off and back on before the Pi has ever sent a time must
  // bring the animation back, not leave the display showing stale content. The
  // off path stops the animation, so the waiting path has to restart it.
  setupChainCapture();
  displayInit();
  displaySetBrightness(0);
  displayTick();
  check(!isLoading(), "turning the display off stops the animation");

  displaySetBrightness(100);
  displayTick();
  check(isLoading(), "turning it back on while still unsynced resumes the animation");
  check(hosttest::lastFrame != std::vector<uint8_t>({0xFF, 0xFF, 0xFF, 0xFF}),
        "and the display is not left blank");
}

// --- Protocol ---------------------------------------------------------------

static void testCRCKnownVector() {
  section("protocol CRC");

  // CRC-8/ATM check value: "123456789" -> 0xF4. The Go side asserts the same
  // constant, which is what holds the two implementations together.
  check(protocolCRC8("123456789", 9) == 0xF4, "CRC-8 check value is 0xF4");
  check(protocolCRC8("", 0) == 0x00, "CRC-8 of empty is 0x00");
}

static void testFrameEncoding() {
  section("protocol encoding");

  hosttest::reset();
  protocolSend2("POT", "0", "742");

  // The exact bytes the Go decoder expects.
  std::string want = std::string(1, 0x02) + "TB1|0|POT|0|742|";
  check(hosttest::serialOut.rfind(want, 0) == 0, "frame starts STX TB1|seq|POT|0|742|");
  check(hosttest::serialOut.back() == '\n', "frame ends with a newline");

  // The checksum covers the body up to but not including the final separator.
  std::string body = "TB1|0|POT|0|742";
  char crc[3];
  snprintf(crc, sizeof(crc), "%02X", protocolCRC8(body.c_str(), body.size()));
  check(hosttest::serialOut == std::string(1, 0x02) + body + "|" + crc + "\n",
        "complete frame matches the Go encoder");

  // The sequence number advances.
  hosttest::serialOut.clear();
  protocolSend2("POT", "1", "331");
  check(hosttest::serialOut.find("TB1|1|POT") != std::string::npos, "sequence advances");
}

static void testFullBufferDropsRatherThanBlocks() {
  section("transmit buffer exhaustion");

  hosttest::reset();
  Serial.setWriteRoom(4096);
  const uint32_t before = protocolDroppedFrames();
  protocolSend2("POT", "0", "742");
  check(!hosttest::serialOut.empty(), "a frame is sent when there is room");
  check(protocolDroppedFrames() == before, "nothing is dropped when there is room");

  // The host has stopped reading, so the USB CDC buffer has no space. The real
  // Serial.write would block here and stall loop() -- which is how the Nano went
  // silent for 15 hours while still enumerating on the bus.
  hosttest::serialOut.clear();
  Serial.setWriteRoom(0);
  protocolSend2("POT", "0", "742");
  check(hosttest::serialOut.empty(), "no bytes are written when the buffer is full");
  check(protocolDroppedFrames() == before + 1, "the dropped frame is counted");

  // A partial frame would desynchronise the Go decoder, so it must be all or
  // nothing: room for most of a frame is still not enough.
  hosttest::serialOut.clear();
  Serial.setWriteRoom(8);
  protocolSend2("POT", "0", "742");
  check(hosttest::serialOut.empty(), "a frame that only partly fits is not started");

  // Once the host drains, sending resumes.
  Serial.setWriteRoom(4096);
  protocolSend2("POT", "0", "742");
  check(!hosttest::serialOut.empty(), "sending resumes once the buffer drains");
}

static void testTxStallDetection() {
  section("transmit stall detection");

  // The fault this exists for: the USB transmit path dies while everything else
  // keeps running. The Nano cannot report that -- the report would travel the
  // broken direction -- so it has to notice that the Pi is still talking while
  // nothing it sends gets out.
  const uint32_t STALL = 30000, FRESH = 10000;

  hosttest::reset();
  Serial.setWriteRoom(4096);
  hosttest::nowMs = 1000;
  protocolResetTxHealth(hosttest::nowMs);

  // Sending normally: not stalled, however long we wait, as long as sends work.
  hosttest::nowMs = 40000;
  protocolSend1("PING", "40000");
  check(!protocolTxStalled(hosttest::nowMs, hosttest::nowMs - 100, STALL, FRESH),
        "a working transmitter is not stalled");

  // The host stops draining. Every frame is now dropped. Twenty pings at the
  // real two-second interval is forty seconds, comfortably past the timeout --
  // ten was not, which is what the first version of this test got wrong.
  Serial.setWriteRoom(0);
  for (int i = 0; i < 20; i++) {
    hosttest::nowMs += 2000;
    protocolSend1("PING", "x");
  }
  check(protocolDroppedFrames() >= 20, "dropped frames are counted while stalled");
  check(protocolTxStalled(hosttest::nowMs, hosttest::nowMs - 100, STALL, FRESH),
        "a transmitter silent past the timeout, while still receiving, is stalled");

  // The Pi going quiet as well is a stopped daemon, not a broken transmitter.
  // Restarting over that would turn every deploy into a reboot.
  check(!protocolTxStalled(hosttest::nowMs, hosttest::nowMs - (FRESH + 1), STALL, FRESH),
        "both directions quiet is not treated as a stall");

  // Never having heard from the Pi is a Nano running on its own, not a fault.
  check(!protocolTxStalled(hosttest::nowMs, 0, STALL, FRESH),
        "a Nano that has never heard the Pi is not stalled");

  // Just short of the timeout is not yet a stall.
  hosttest::nowMs = 100000;
  protocolResetTxHealth(hosttest::nowMs);
  hosttest::nowMs += STALL - 1;
  check(!protocolTxStalled(hosttest::nowMs, hosttest::nowMs - 100, STALL, FRESH),
        "a transmitter inside the timeout is not stalled");
  hosttest::nowMs += 2;
  check(protocolTxStalled(hosttest::nowMs, hosttest::nowMs - 100, STALL, FRESH),
        "crossing the timeout is a stall");

  // Recovery: once sending works again the stall clears on its own.
  Serial.setWriteRoom(4096);
  protocolSend1("PING", "ok");
  check(!protocolTxStalled(hosttest::nowMs, hosttest::nowMs - 100, STALL, FRESH),
        "a successful send clears the stall");

  // The bug that restarted the Nano during boot. loop() samples millis() once,
  // then sends a PING; if the clock ticks during the write, the frame is stamped
  // a millisecond *after* the `now` the stall check is given. Unsigned, that
  // difference was 4.29 billion milliseconds, so a working transmitter that had
  // just sent a PING was declared dead and restarted. Busy Pi, slow USB drain,
  // write crosses a millisecond: twenty seconds into every boot.
  hosttest::nowMs = 50001;
  protocolSend1("PING", "50000");
  check(!protocolTxStalled(50000, 49990, STALL, FRESH),
        "a frame sent a millisecond after now was sampled is not a stall");
  check(protocolMsSinceTxOk(50000) == 0,
        "and it counts as sent zero milliseconds ago, not 4.29 billion");

  // millis() wraps at 49.7 days. A frame sent just before the wrap, checked just
  // after it, is recent.
  hosttest::nowMs = 0xFFFFFFF0u;
  protocolSend1("PING", "wrap");
  check(!protocolTxStalled(0x00000010u, 0x00000008u, STALL, FRESH),
        "a frame sent just before millis() wrapped is recent, not stale");
  check(protocolMsSinceTxOk(0x00000010u) == 0x20,
        "and the elapsed time across the wrap is right");
}

// --- Shutdown by holding the big red button ---------------------------------

// The four cells of a frame as the driver shifts them out: last cell first, each
// byte active low, every decimal point dark.
static std::vector<uint8_t> frameOf(const char* four) {
  const uint8_t DOT = 0b00000001;
  std::vector<uint8_t> v;
  for (int i = 3; i >= 0; i--) v.push_back((uint8_t)(~getCharacter(four[i]) | DOT));
  return v;
}

static uint32_t piHeard = 1;   // when the Pi last spoke; non-zero means "connected"

static void sdStep(uint32_t ms, bool down, bool alarm = false) {
  hosttest::nowMs = ms;
  piHeard = ms;   // a connected Pi speaks constantly
  shutdownTick(ms, down, alarm, piHeard);
  displayTick();
}

static bool sentRequest() {
  return hosttest::serialOut.find("|SHUTDOWN|REQUEST|") != std::string::npos;
}

static int requestCount() {
  int n = 0;
  for (size_t at = 0; (at = hosttest::serialOut.find("|SHUTDOWN|REQUEST|", at)) != std::string::npos; at++) n++;
  return n;
}

// sdBegin brings a synced display up with the button seen released, which is the
// state of any Timeblaster that has been running for more than a moment.
static void sdBegin() {
  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displaySetBrightness(75);
  displayShowClock();
  displaySetTime(22, 40);
  Serial.setWriteRoom(4096);
  shutdownInit();
  sdStep(1000, false);              // released, after the settle window
  hosttest::serialOut.clear();
}

static void testShutdownCountdown() {
  section("shutdown countdown");
  sdBegin();

  // Pressed at 2000. Nothing shows for the first two seconds.
  sdStep(2000, true);
  sdStep(3500, true);
  check(!displayShutdownActive(), "an ordinary press shows nothing");

  // From two seconds in, the countdown: 8, 7, ... 1, 0.
  const char* want[] = {"   8", "   7", "   6", "   5", "   4", "   3", "   2", "   1", "   0"};
  for (int i = 0; i <= 8; i++) {
    sdStep(2000 + 2000 + i * 1000, true);
    char what[64];
    snprintf(what, sizeof(what), "%d seconds held shows %s", 2 + i, want[i]);
    checkBytes(hosttest::lastFrame, frameOf(want[i]), what);
  }
  check(!sentRequest(), "nothing is sent while the countdown runs");

  // Half a second on "0", then the request and the animation.
  sdStep(2000 + 10000 + SHUTDOWN_ZERO_MS, true);
  check(sentRequest(), "the request is sent once the countdown has run out");
  check(shutdownPhase() == SHUTDOWN_REQUESTED, "and the machine waits for the Pi");
  check(isLoading(), "the loading animation runs while it waits");

  // Held on past the end, ticking along: still exactly one request.
  for (uint32_t t = 12600; t < 14000; t += 100) sdStep(t, true);
  check(requestCount() == 1, "only one request per hold");
}

static void testShutdownCancelsOnRelease() {
  section("shutdown cancels on release");
  sdBegin();

  sdStep(2000, true);
  sdStep(7000, true);   // showing "5"
  check(displayShutdownActive(), "the countdown is showing");

  sdStep(7100, false);
  check(shutdownPhase() == SHUTDOWN_IDLE, "letting go cancels");
  check(!displayShutdownActive(), "and the display goes back to normal");
  check(!sentRequest(), "and nothing was sent");
  check(!isLoading(), "with no animation left running");
}

static void testShutdownNeverFromAnAlarmPress() {
  section("shutdown never from silencing an alarm");

  // Someone half-asleep leaning on the button to stop the alarm must not switch
  // the alarm clock off, however long they lean.
  //
  // The alarm is ringing only at the instant of the press and stops on the very
  // next pass -- dismissed from the app, or timing out on its own. That is the
  // case only this rule covers: an alarm still ringing a second later would be
  // caught by the mid-countdown check anyway, and the first version of this test
  // passed for that reason with the rule deleted.
  sdBegin();
  sdStep(2000, true, /*alarm=*/true);
  for (uint32_t t = 2050; t <= 20000; t += 250) sdStep(t, true, /*alarm=*/false);
  check(!sentRequest(), "a press that began while ringing never shuts down");
  check(!displayShutdownActive(), "and never shows a countdown");

  // An alarm starting part-way through cancels, and the same press stays dead.
  sdBegin();
  sdStep(2000, true);
  sdStep(6000, true);
  check(displayShutdownActive(), "counting down");
  sdStep(6100, true, /*alarm=*/true);
  check(shutdownPhase() == SHUTDOWN_IDLE, "an alarm starting cancels the countdown");
  for (uint32_t t = 6200; t <= 20000; t += 250) sdStep(t, true, false);
  check(!sentRequest(), "and holding on afterwards does not resume it");

  // Letting go and pressing again is a fresh, deliberate press.
  sdStep(20100, false);
  sdStep(21000, true);
  sdStep(21000 + SHUTDOWN_HOLD_MS + SHUTDOWN_ZERO_MS, true);
  check(sentRequest(), "a new press afterwards works normally");
}

static void testShutdownIgnoresAButtonHeldSinceBoot() {
  section("shutdown ignores a button held since power-on");

  // Stuck, or held down by something resting on it. Without this it would shut
  // the Timeblaster down ten seconds after every boot.
  setupChainCapture();
  displayInit();
  displaySetSynced(true);
  displayShowClock();
  Serial.setWriteRoom(4096);
  shutdownInit();
  hosttest::serialOut.clear();

  for (uint32_t t = 0; t <= 30000; t += 100) sdStep(t, true);
  check(!sentRequest(), "a button never seen released cannot shut down");
  check(!displayShutdownActive(), "or show a countdown");

  // The debounce reports "up" for its first few milliseconds whatever the pin
  // says. That must not count as having seen it released.
  shutdownInit();
  sdStep(0, false);
  sdStep(10, false);
  for (uint32_t t = 30; t <= 30000; t += 100) sdStep(t, true);
  check(!sentRequest(), "the debounce's start-up \"up\" is not a release");

  // Once it has genuinely been let go, holding it works.
  sdStep(30100, false);
  sdStep(31000, true);
  sdStep(31000 + SHUTDOWN_HOLD_MS + SHUTDOWN_ZERO_MS, true);
  check(sentRequest(), "after a real release, a hold works");
}

static void testShutdownAnswers() {
  section("shutdown answers from the Pi");

  // Accepted: the animation stays, however long the Pi takes to go.
  sdBegin();
  sdStep(2000, true);
  sdStep(2000 + SHUTDOWN_HOLD_MS + SHUTDOWN_ZERO_MS, true);
  shutdownAnswer(true, hosttest::nowMs);
  check(shutdownPhase() == SHUTDOWN_CONFIRMED, "accepted");
  for (uint32_t t = 13000; t <= 13000 + SHUTDOWN_ANSWER_MS * 2; t += 500) sdStep(t, false);
  check(shutdownPhase() == SHUTDOWN_CONFIRMED && isLoading(),
        "once accepted, the animation does not time out");

  // The Pi goes quiet and USB stayed powered: say it is off rather than going
  // back to a clock that would look like it is still running.
  uint32_t lastHeard = hosttest::nowMs;
  for (uint32_t t = lastHeard; t <= lastHeard + SHUTDOWN_SILENT_MS + 1000; t += 500) {
    hosttest::nowMs = t;
    shutdownTick(t, false, false, lastHeard);
    displayTick();
  }
  check(shutdownPhase() == SHUTDOWN_OFF, "a silent Pi after accepting means off");
  checkBytes(hosttest::lastFrame, frameOf(" OFF"), "and the display says OFF");

  // It comes back without the Nano losing power: back to normal.
  uint32_t back = hosttest::nowMs + 500;
  hosttest::nowMs = back;
  shutdownTick(back, false, false, back);
  displayTick();
  check(shutdownPhase() == SHUTDOWN_IDLE && !displayShutdownActive(),
        "hearing from the Pi again ends the OFF state");

  // Refused: straight back to normal.
  sdBegin();
  sdStep(2000, true);
  sdStep(2000 + SHUTDOWN_HOLD_MS + SHUTDOWN_ZERO_MS, true);
  shutdownAnswer(false, hosttest::nowMs);
  check(shutdownPhase() == SHUTDOWN_IDLE && !displayShutdownActive(), "refused goes back to normal");
  check(!isLoading(), "with the animation stopped");
}

static void testShutdownSurvivesALostRequest() {
  section("shutdown survives a lost request");

  // The transmit path has died before. A request that never left must not leave
  // an animation running that looks like a shutdown in progress.
  sdBegin();
  Serial.setWriteRoom(0);
  sdStep(2000, true);
  sdStep(2000 + SHUTDOWN_HOLD_MS + SHUTDOWN_ZERO_MS, true);
  check(!sentRequest(), "the request was dropped by the full buffer");
  check(shutdownPhase() == SHUTDOWN_REQUESTED, "but the machine believes it asked");

  uint32_t asked = hosttest::nowMs;
  sdStep(asked + SHUTDOWN_ANSWER_MS - 100, true);
  check(shutdownPhase() == SHUTDOWN_REQUESTED, "it waits for an answer");
  sdStep(asked + SHUTDOWN_ANSWER_MS, true);
  check(shutdownPhase() == SHUTDOWN_IDLE && !displayShutdownActive() && !isLoading(),
        "no answer in time goes back to normal");

  // Still holding: that press is spent, so it does not start straight over.
  Serial.setWriteRoom(4096);
  for (uint32_t t = asked + SHUTDOWN_ANSWER_MS; t <= asked + 30000; t += 250) sdStep(t, true);
  check(!sentRequest(), "the same press does not try again");
}

static void testShutdownShowsWithTheDisplayOff() {
  section("shutdown shows with the display off");
  sdBegin();
  displaySetBrightness(0);
  sdStep(2000, true);
  sdStep(5000, true);
  checkBytes(hosttest::lastFrame, frameOf("   7"),
             "the countdown shows even with the display switched off");
  sdStep(5100, false);
  displayTick();
  checkBytes(hosttest::lastFrame, frameOf("    "), "and the display goes back to off");
}

static void testWiFiButtonCannotArmShutdown() {
  section("the Wi-Fi button cannot arm a shutdown");

  // Through the real debounce, not a hand-fed flag: the countdown reads the big
  // red button by name, and holding the Wi-Fi button for any length of time --
  // its own five-second hold included -- must leave it untouched.
  sdBegin();
  hosttest::pinState[BUTTON_ALARM_OFF_PIN] = HIGH;   // active low: released
  hosttest::pinState[BUTTON_WIFI_PIN] = HIGH;
  inputsInit();
  hosttest::nowMs = 1000;
  inputsPoll();
  hosttest::nowMs = 1050;
  inputsPoll();
  hosttest::serialOut.clear();

  hosttest::pinState[BUTTON_WIFI_PIN] = LOW;          // Wi-Fi held
  for (uint32_t t = 2000; t <= 20000; t += 50) {
    hosttest::nowMs = t;
    inputsPoll();
    shutdownPoll(t, t);   // the same call loop() makes
    displayTick();
  }
  check(inputsButtonDown("WIFI"), "the Wi-Fi button reads as held");
  check(!inputsButtonDown("ALARM_OFF"), "the big red button does not");
  check(!sentRequest() && !displayShutdownActive(),
        "eighteen seconds on the Wi-Fi button never starts a shutdown");
  check(hosttest::serialOut.find("|BUTTON|WIFI|DOWN|") != std::string::npos,
        "and its press still reaches the Pi, which owns its five-second hold");
}

static void testFrameEscaping() {
  section("protocol escaping");

  hosttest::reset();
  protocolSend2("LOG", "warn", "a|b\\c\nd");
  check(hosttest::serialOut.find("a\\pb\\\\c\\nd") != std::string::npos,
        "separators, backslashes and newlines are escaped");
  // Exactly one newline: the terminator.
  check(std::count(hosttest::serialOut.begin(), hosttest::serialOut.end(), '\n') == 1,
        "an escaped newline does not terminate the frame early");
}

static std::vector<Message> received;
static void collect(const Message& m) { received.push_back(m); }

// feed encodes a frame the way the Pi would and hands it to the parser.
static void feed(const std::string& body) {
  char crc[3];
  snprintf(crc, sizeof(crc), "%02X", protocolCRC8(body.c_str(), body.size()));
  hosttest::serialIn = std::string(1, 0x02) + body + "|" + crc + "\n";
  Serial.resetReader();
  protocolPoll(collect);
}

static void testFrameDecoding() {
  section("protocol decoding");

  hosttest::reset();
  received.clear();
  feed("TB1|1|TIME|1758220642|-14400|250");

  check(received.size() == 1, "one message decoded");
  if (!received.empty()) {
    check(received[0].type == "TIME", "type is TIME");
    check(received[0].argc == 3, "three arguments");
    check(received[0].args[0] == "1758220642", "unix seconds");
    check(received[0].args[1] == "-14400", "utc offset");
    check(received[0].args[2] == "250", "milliseconds");
  }

  // Display text survives escaping round-trip.
  received.clear();
  hosttest::reset();
  feed("TB1|2|DISPLAY|TEXT|SETUP");
  check(received.size() == 1 && received[0].args[1] == "SETUP", "DISPLAY TEXT decodes");
}

static void testCorruptFramesAreDropped() {
  section("protocol robustness");

  received.clear();
  hosttest::reset();

  // A valid frame with one payload byte flipped.
  std::string body = "TB1|1|POT|0|742";
  char crc[3];
  snprintf(crc, sizeof(crc), "%02X", protocolCRC8(body.c_str(), body.size()));
  std::string corrupt = std::string(1, 0x02) + "TB1|1|POT|0|743|" + crc + "\n";

  // Garbage, then the corrupt frame, then a good one.
  std::string good_body = "TB1|2|PONG";
  char gcrc[3];
  snprintf(gcrc, sizeof(gcrc), "%02X", protocolCRC8(good_body.c_str(), good_body.size()));

  hosttest::serialIn = "ESP-ROM:esp32s3\r\nrst:0x1\r\n" + corrupt +
                       std::string(1, 0x02) + good_body + "|" + gcrc + "\n";
  Serial.resetReader();
  protocolPoll(collect);

  check(received.size() == 1, "the corrupt frame is dropped and the good one survives");
  if (received.size() == 1) {
    check(received[0].type == "PONG", "resynchronised onto the next frame");
  }

  // An unknown protocol version is ignored.
  received.clear();
  hosttest::reset();
  std::string v9 = "TB9|1|PONG";
  char vcrc[3];
  snprintf(vcrc, sizeof(vcrc), "%02X", protocolCRC8(v9.c_str(), v9.size()));
  hosttest::serialIn = std::string(1, 0x02) + v9 + "|" + vcrc + "\n";
  Serial.resetReader();
  protocolPoll(collect);
  check(received.empty(), "an unknown protocol version is ignored");
}

// --- Inputs -----------------------------------------------------------------

static void testPotReporting() {
  section("potentiometer reporting");

  hosttest::reset();
  inputsInit();

  // The first sample of each pot is reported immediately, so the knob's
  // position at boot is known without waiting for someone to touch it.
  hosttest::analogValue[A0] = 2048;
  hosttest::analogValue[A1] = 1000;
  hosttest::analogValue[A2] = 0;
  hosttest::analogValue[A3] = 4095;
  hosttest::nowMs = 100;
  inputsPoll();

  check(hosttest::serialOut.find("|POT|0|2048|") != std::string::npos,
        "pot 0 reported verbatim on the first sample");
  check(hosttest::serialOut.find("|POT|3|4095|") != std::string::npos,
        "pot 3 reported on the first sample");

  // Noise below the threshold is swallowed.
  hosttest::serialOut.clear();
  for (int i = 0; i < 5; i++) {
    hosttest::analogValue[A0] = 2048 + ((i % 2) ? 3 : -3);
    hosttest::nowMs += 25;
    inputsPoll();
  }
  check(hosttest::serialOut.find("|POT|0|") == std::string::npos,
        "ADC noise below the threshold is not reported");

  // A deliberate move is.
  hosttest::serialOut.clear();
  for (int i = 0; i < 20; i++) {
    hosttest::analogValue[A0] = 3000;
    hosttest::nowMs += 25;
    inputsPoll();
  }
  check(hosttest::serialOut.find("|POT|0|") != std::string::npos,
        "a deliberate move is reported");
}

static void testButtonDebounce() {
  section("button debounce");

  hosttest::reset();
  inputsInit();
  hosttest::nowMs = 1000;

  // Buttons are active low, so HIGH is released.
  hosttest::pinState[BUTTON_ALARM_OFF_PIN] = HIGH;
  hosttest::pinState[BUTTON_WIFI_PIN] = HIGH;
  inputsPoll();
  hosttest::serialOut.clear();

  // Press, then poll again past the debounce window.
  hosttest::pinState[BUTTON_ALARM_OFF_PIN] = LOW;
  inputsPoll();                       // records the edge, does not report yet
  check(hosttest::serialOut.find("BUTTON") == std::string::npos,
        "an edge is not reported before the debounce window elapses");

  hosttest::nowMs += 30;
  inputsPoll();
  check(hosttest::serialOut.find("|BUTTON|ALARM_OFF|DOWN|") != std::string::npos,
        "the press is reported once debounced");

  // Release.
  hosttest::serialOut.clear();
  hosttest::pinState[BUTTON_ALARM_OFF_PIN] = HIGH;
  inputsPoll();
  hosttest::nowMs += 30;
  inputsPoll();
  check(hosttest::serialOut.find("|BUTTON|ALARM_OFF|UP|") != std::string::npos,
        "the release is reported");

  // A bounce shorter than the window produces nothing.
  hosttest::serialOut.clear();
  hosttest::pinState[BUTTON_WIFI_PIN] = LOW;
  inputsPoll();
  hosttest::nowMs += 5;
  hosttest::pinState[BUTTON_WIFI_PIN] = HIGH;
  inputsPoll();
  hosttest::nowMs += 30;
  inputsPoll();
  check(hosttest::serialOut.find("BUTTON") == std::string::npos,
        "contact bounce is swallowed");
}

static void testPinsDoNotCollide() {
  section("pin assignments");

  // The display's three pins are fixed by the PCB. Nothing else may claim them:
  // an LED on DATA or CLOCK would corrupt every frame, and this is exactly the
  // kind of mistake that is invisible until the hardware is assembled.
  uint8_t display[] = {DISPLAY_DATA_PIN, DISPLAY_CLOCK_PIN, DISPLAY_LATCH_PIN};
  uint8_t others[] = {
    LED_ALARM_PIN, LED_WIFI_PIN, LED_POWER_PIN,
    BUTTON_WIFI_PIN, BUTTON_ALARM_OFF_PIN,
    POT_PINS[0], POT_PINS[1], POT_PINS[2], POT_PINS[3],
  };

  for (uint8_t d : display) {
    for (uint8_t o : others) {
      checks++;
      if (d == o) {
        failures++;
        printf("  FAIL  pin %u is claimed by both the display and another peripheral\n", d);
      }
    }
  }

  // And nothing else collides with itself either.
  for (size_t i = 0; i < sizeof(others); i++) {
    for (size_t j = i + 1; j < sizeof(others); j++) {
      checks++;
      if (others[i] == others[j]) {
        failures++;
        printf("  FAIL  pin %u is claimed twice\n", others[i]);
      }
    }
  }
}

int main() {
  printf("Timeblaster firmware host tests\n");

  testCharacterTable();
  testFramePolarityAndOrder();
  testBlankAndFullFrames();
  testDigitCountClamping();
  testScrolling();
  testClockRendering();
  testClock24h();
  testAlarmPendingDot();
  testOverrideTextAndReturn();
  testBrightnessZeroBlanks();
  testAlarmFlash();
  testLoadingUntilSynced();
  testCRCKnownVector();
  testFrameEncoding();
  testFullBufferDropsRatherThanBlocks();
  testTxStallDetection();
  testShutdownCountdown();
  testShutdownCancelsOnRelease();
  testShutdownNeverFromAnAlarmPress();
  testShutdownIgnoresAButtonHeldSinceBoot();
  testShutdownAnswers();
  testShutdownSurvivesALostRequest();
  testShutdownShowsWithTheDisplayOff();
  testWiFiButtonCannotArmShutdown();
  testFrameEscaping();
  testFrameDecoding();
  testCorruptFramesAreDropped();
  testPotReporting();
  testButtonDebounce();
  testPinsDoNotCollide();

  printf("\n%d checks, %d failures\n", checks, failures);
  return failures == 0 ? 0 : 1;
}
