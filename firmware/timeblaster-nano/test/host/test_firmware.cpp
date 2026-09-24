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
  testOverrideTextAndReturn();
  testBrightnessZeroBlanks();
  testAlarmFlash();
  testLoadingUntilSynced();
  testCRCKnownVector();
  testFrameEncoding();
  testFrameEscaping();
  testFrameDecoding();
  testCorruptFramesAreDropped();
  testPotReporting();
  testButtonDebounce();
  testPinsDoNotCollide();

  printf("\n%d checks, %d failures\n", checks, failures);
  return failures == 0 ? 0 : 1;
}
