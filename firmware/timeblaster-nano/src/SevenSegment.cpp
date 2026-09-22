#include "SevenSegment.h"
#include <Arduino.h>
#include <string.h>

const int DATA_PIN  = D5;
const int CLOCK_PIN = D6;
const int LATCH_PIN = D7;

// The byte clocked out first travels furthest down the daisy chain, so on this
// build it lands on the RIGHTmost digit. Shifting the cells out back-to-front
// therefore puts cell 0 on the left, where it reads naturally. Flip this if the
// chain is ever rewired the other way.
const bool CHAIN_RUNS_RIGHT_TO_LEFT = true;

const int DEFAULT_SCROLL_SPEED = 200; // milliseconds per scroll step
const size_t DISPLAY_BUFFER_SIZE = 64;  // max characters of content
const int DEFAULT_DISPLAY_DIGITS = 4;   // digits assumed if none is given
const int MAX_DISPLAY_DIGITS = 8;       // upper bound on the daisy chain
const int DEFAULT_LOADING_FRAME_MS = 80; // milliseconds per animation step
const int LOADING_COMET_LENGTH = 3;     // segments lit at once, head included

const uint8_t DOT_BIT = 0b0000001;

uint8_t getCharacter(char c) {
  switch (c) {
    // Numbers
    case '0': return 0b11101110;
    case '1': return 0b01001000;
    case '2': return 0b11010110;
    case '3': return 0b11011100;
    case '4': return 0b01111000;
    case '5': return 0b10111100;
    case '6': return 0b10111110;
    case '7': return 0b11001000;
    case '8': return 0b11111110;
    case '9': return 0b11111000;

    // A
    case 'A':
    case 'a':
      return 0b11111010;

    // B - lowercase b looks better on 7-segment
    case 'B':
    case 'b':
      return 0b00111110;

    // C
    case 'C':
      return 0b10100110;
    case 'c':
      return 0b00010110;

    // D - lowercase d representation
    case 'D':
    case 'd':
      return 0b01011110;

    // E
    case 'E':
    case 'e':
      return 0b10110110;

    // F
    case 'F':
    case 'f':
      return 0b10110010;

    // G
    case 'G':
    case 'g':
      return 0b10111100;

    // H
    case 'H':
      return 0b01111010;
    case 'h':
      return 0b00111010;

    // I
    case 'I':
      return 0b01001000;
    case 'i':
      return 0b00001000;

    // J
    case 'J':
    case 'j':
      return 0b01001110;

    // K - approximation (looks like H)
    case 'K':
    case 'k':
      return 0b01111010;

    // L
    case 'L':
    case 'l':
      return 0b00100110;

    // M - very rough approximation
    case 'M':
    case 'm':
      return 0b11101010;

    // N - lowercase n works better
    case 'N':
    case 'n':
      return 0b00011010;

    // O
    case 'O':
      return 0b11101110;
    case 'o':
      return 0b00011110;

    // P
    case 'P':
    case 'p':
      return 0b11110010;

    // Q - lowercase q approximation
    case 'Q':
    case 'q':
      return 0b11111000;

    // R - lowercase r
    case 'R':
    case 'r':
      return 0b00010010;

    // S
    case 'S':
    case 's':
      return 0b10111100;

    // T - lowercase t
    case 'T':
    case 't':
      return 0b00110110;

    // U
    case 'U':
      return 0b01101110;
    case 'u':
      return 0b00001110;

    // V - approximation
    case 'V':
    case 'v':
      return 0b00101110;

    // W - approximation (same basic shape as U)
    case 'W':
    case 'w':
      return 0b01101110;

    // X - approximation (same as H)
    case 'X':
    case 'x':
      return 0b01111010;

    // Y
    case 'Y':
    case 'y':
      return 0b01111100;

    // Z - same shape as 2
    case 'Z':
    case 'z':
      return 0b11010110;

    // Symbols
    case '-':
      return 0b00010000;

    case '_':
      return 0b00000100;

    case '=':
      return 0b00010100;

    case ' ':
      return 0b00000000;

    // Unknown character = blank
    default:
      return 0b00000000;
  }
}

void iShiftOut(
  uint8_t dataPin,
  uint8_t clockPin,
  uint8_t bitOrder,
  uint8_t val
) {
  for (uint8_t i = 0; i < 8; i++) {
    uint8_t bitToSend;

    if (bitOrder == LSBFIRST) {
      bitToSend = (val >> i) & 0x01;
    } else {
      bitToSend = (val >> (7 - i)) & 0x01;
    }

    digitalWrite(dataPin, bitToSend);

    // Small settling delay for the Nano ESP32 / 74HC595.
    delayMicroseconds(2);

    digitalWrite(clockPin, HIGH);
    delayMicroseconds(2);

    digitalWrite(clockPin, LOW);
    delayMicroseconds(2);
  }
}

// Shifts one digit onto the chain without touching the latch, so callers can
// push several digits and land them all in the same latch pulse.
static void shiftDigit(uint8_t digit, bool withDot) {
  // flip the bits so 1 equals LOW and 0 equals HIGH
  digit = (~digit);

  // The dot is active low too, like the segments: clear the bit to light it.
  if (withDot) {
    digit = digit & ~DOT_BIT;
  } else {
    digit = digit | DOT_BIT;
  }

  iShiftOut(
    DATA_PIN,
    CLOCK_PIN,
    MSBFIRST,
    digit
  );
}

// The single place that decides which cell lands on which physical digit.
// Everything that draws a full frame goes through here.
static void pushFrame(const uint8_t* segments, const bool* dots, int count) {
  digitalWrite(LATCH_PIN, LOW);

  for (int i = 0; i < count; i++) {
    int cell = CHAIN_RUNS_RIGHT_TO_LEFT ? (count - 1 - i) : i;
    shiftDigit(segments[cell], dots != nullptr && dots[cell]);
  }

  digitalWrite(LATCH_PIN, HIGH);
}

void setClock(
    uint8_t firstDigit, 
    uint8_t secondDigit, 
    uint8_t thirdDigit, 
    uint8_t fourthDigit, 
    bool firstDot, 
    bool secondDot, 
    bool thirdDot, 
    bool fourthDot
  ) {
  uint8_t segments[4] = {firstDigit, secondDigit, thirdDigit, fourthDigit};
  bool dots[4] = {firstDot, secondDot, thirdDot, fourthDot};
  pushFrame(segments, dots, 4);
}

void setSingleDigit(uint8_t digit, bool withDot) {
  digitalWrite(LATCH_PIN, LOW);
  shiftDigit(digit, withDot);
  digitalWrite(LATCH_PIN, HIGH);
}

// ---------------------------------------------------------------------------
// Display state
//
// The display is retained-mode: callers say what should be on it, and
// paintDisplay() keeps the hardware matching that. Content that fits across
// the connected digits sits still; anything longer scrolls on its own.
// ---------------------------------------------------------------------------

// The content as the caller gave it, kept so setDisplayText can tell a real
// change from the same value being pushed again.
static char displayText[DISPLAY_BUFFER_SIZE] = {0};

// The content decoded into cells. A cell is one physical digit: its segment
// pattern plus whether its decimal point is lit.
static uint8_t displaySegments[DISPLAY_BUFFER_SIZE];
static bool    displayDots[DISPLAY_BUFFER_SIZE];
static size_t  displayCellCount = 0;

// How many 74HC595s are actually on the chain. Set by
// initializeSevenSegmentDisplay(); everything below sizes itself to this.
static int           displayDigitCount = DEFAULT_DISPLAY_DIGITS;

static bool          displayScrolling = false;
static bool          displayNeedsPaint = true;
static int           displayIndex = 0;
static int           displayScrollSpeed = DEFAULT_SCROLL_SPEED;
static unsigned long displayLastScroll = 0;

// Decodes text into cells. A '.' binds to the character before it rather than
// taking a cell of its own, so "12.03" is four digits with a lit decimal
// point, not five characters.
static void parseDisplayText(const char* text) {
  displayCellCount = 0;

  for (size_t i = 0; text[i] != '\0' && displayCellCount < DISPLAY_BUFFER_SIZE; i++) {
    char c = text[i];

    if (c == '.' && displayCellCount > 0 && !displayDots[displayCellCount - 1]) {
      displayDots[displayCellCount - 1] = true;
      continue;
    }

    // A leading '.' or a second '.' in a row has no character to attach to,
    // so it gets a blank cell of its own.
    if (c == '.') {
      displaySegments[displayCellCount] = getCharacter(' ');
      displayDots[displayCellCount] = true;
      displayCellCount++;
      continue;
    }

    displaySegments[displayCellCount] = getCharacter(c);
    displayDots[displayCellCount] = false;
    displayCellCount++;
  }
}

// Pushes the displayDigitCount cells starting at `start` to the hardware,
// all inside one latch pulse. While scrolling, the window wraps over the
// content plus a displayDigitCount blank tail -- that tail is what lets text
// march all the way off the left before the wrap brings it back in from the
// right.
static void pushWindow(size_t start) {
  size_t virtualLength = displayCellCount + displayDigitCount;

  uint8_t segments[MAX_DISPLAY_DIGITS];
  bool dots[MAX_DISPLAY_DIGITS];

  for (int i = 0; i < displayDigitCount; i++) {
    size_t position = displayScrolling ? (start + i) % virtualLength : start + i;

    if (position < displayCellCount) {
      segments[i] = displaySegments[position];
      dots[i] = displayDots[position];
    } else {
      segments[i] = getCharacter(' ');
      dots[i] = false;
    }
  }

  pushFrame(segments, dots, displayDigitCount);
}

// ---------------------------------------------------------------------------
// Loading animation
//
// A comet that laps the outside edge of the display, treating all the digits
// as one canvas rather than as separate spinners: it runs along the top from
// left to right, down the right-hand edge of the last digit, back along the
// bottom, and up the left-hand edge of the first one. Built from raw segments,
// so it owes nothing to the character table.
//
// The lap is 2 * digits + 4 steps long. At one digit that degrades on its own
// into the familiar single-digit spinner.
// ---------------------------------------------------------------------------

static bool          loadingActive = false;
static int           loadingIndex = 0;
static int           loadingFrameMs = DEFAULT_LOADING_FRAME_MS;
static unsigned long loadingLastFrame = 0;

static int loadingStepCount() {
  return 2 * displayDigitCount + 4;
}

// Maps one step of the lap to the digit and segment it lights.
static void loadingStepAt(int step, int* digit, uint8_t* segment) {
  int last = displayDigitCount - 1;

  if (step < displayDigitCount) {          // along the top, left to right
    *digit = step;
    *segment = SEG_A;
  } else if (step == displayDigitCount) {  // down the right-hand edge
    *digit = last;
    *segment = SEG_B;
  } else if (step == displayDigitCount + 1) {
    *digit = last;
    *segment = SEG_C;
  } else if (step < 2 * displayDigitCount + 2) {  // back along the bottom
    *digit = last - (step - (displayDigitCount + 2));
    *segment = SEG_D;
  } else if (step == 2 * displayDigitCount + 2) { // up the left-hand edge
    *digit = 0;
    *segment = SEG_E;
  } else {
    *digit = 0;
    *segment = SEG_F;
  }
}

static void pushLoadingFrame() {
  uint8_t seg[MAX_DISPLAY_DIGITS] = {0};
  int steps = loadingStepCount();

  // The head plus a short tail behind it, so it reads as motion instead of a
  // single segment blinking from place to place.
  for (int t = 0; t < LOADING_COMET_LENGTH && t < steps; t++) {
    int step = ((loadingIndex - t) % steps + steps) % steps;

    int digit;
    uint8_t segment;
    loadingStepAt(step, &digit, &segment);
    seg[digit] |= segment;
  }

  pushFrame(seg, nullptr, displayDigitCount);
}

void setLoading(bool on, int frameMs) {
  if (frameMs <= 0) {
    frameMs = DEFAULT_LOADING_FRAME_MS;
  }
  loadingFrameMs = frameMs;

  // Toggling to the state it is already in leaves the lap running, so this is
  // safe to call every pass of loop().
  if (on == loadingActive) {
    return;
  }
  loadingActive = on;

  if (on) {
    loadingIndex = 0;
    // Back-date the frame clock so the first frame lands on the next paint
    // rather than one interval later.
    loadingLastFrame = millis() - (unsigned long)loadingFrameMs;
  } else {
    // Hand the display back to whatever setDisplayText() last set, and give a
    // scroll in progress a fresh clock so it does not jump on resume.
    displayNeedsPaint = true;
    displayLastScroll = millis();
  }
}

void setLoading(bool on) {
  setLoading(on, loadingFrameMs);
}

bool isLoading() {
  return loadingActive;
}

void setDisplayText(const char* text, int scrollSpeed) {
  if (text == nullptr) {
    text = "";
  }
  if (scrollSpeed <= 0) {
    scrollSpeed = DEFAULT_SCROLL_SPEED;
  }

  // Setting the same content again is a no-op, so callers can push the current
  // value every pass of loop() without restarting a scroll that's in progress.
  if (scrollSpeed == displayScrollSpeed &&
      strncmp(displayText, text, DISPLAY_BUFFER_SIZE - 1) == 0) {
    return;
  }

  strncpy(displayText, text, DISPLAY_BUFFER_SIZE - 1);
  displayText[DISPLAY_BUFFER_SIZE - 1] = '\0';
  displayScrollSpeed = scrollSpeed;

  parseDisplayText(displayText);

  displayScrolling = (displayCellCount > (size_t)displayDigitCount);
  displayIndex = 0;
  displayLastScroll = millis();
  displayNeedsPaint = true;
}

void setDisplayText(const char* text) {
  setDisplayText(text, displayScrollSpeed);
}

void clearDisplay() {
  displayText[0] = '\0';
  displayCellCount = 0;
  displayScrolling = false;
  displayIndex = 0;
  displayNeedsPaint = true;
}

int getDisplayDigitCount() {
  return displayDigitCount;
}

bool isDisplayScrolling() {
  return displayScrolling;
}

void paintDisplay() {
  if (loadingActive) {
    if (millis() - loadingLastFrame >= (unsigned long)loadingFrameMs) {
      loadingLastFrame = millis();
      pushLoadingFrame();
      loadingIndex = (loadingIndex + 1) % loadingStepCount();
    }
    return;
  }

  if (displayScrolling) {
    // Unsigned subtraction, so this stays correct across the millis() rollover
    // at ~49 days.
    if (millis() - displayLastScroll >= (unsigned long)displayScrollSpeed) {
      displayLastScroll = millis();
      displayIndex = (displayIndex + 1) % (int)(displayCellCount + displayDigitCount);
      displayNeedsPaint = true;
    }
  }

  // Static content only reaches the hardware when it actually changed. A full
  // four-digit update is ~200us of bit-banging, so this keeps a steady display
  // from burning that on every pass of loop().
  if (!displayNeedsPaint) {
    return;
  }

  displayNeedsPaint = false;
  pushWindow((size_t)displayIndex);
}


void initializeSevenSegmentDisplay(int digitCount) {
    // Clamped rather than rejected: a bad value here would otherwise show up
    // much later as a blank or garbled display.
    if (digitCount < 1) {
        digitCount = 1;
    } else if (digitCount > MAX_DISPLAY_DIGITS) {
        digitCount = MAX_DISPLAY_DIGITS;
    }
    displayDigitCount = digitCount;
    loadingActive = false;

    pinMode(DATA_PIN, OUTPUT);
    pinMode(CLOCK_PIN, OUTPUT);
    pinMode(LATCH_PIN, OUTPUT);
    
    digitalWrite(LATCH_PIN, LOW);
    digitalWrite(DATA_PIN, LOW);
    digitalWrite(CLOCK_PIN, LOW);

    clearDisplay();
    paintDisplay();
}