#ifndef SEVEN_SEGMENT_H
#define SEVEN_SEGMENT_H

#include <Arduino.h>

// Call once from setup(), passing however many digits are actually wired up
// (1..8). Everything else sizes itself to that.
void initializeSevenSegmentDisplay(int digitCount = 4);

// --- Display state -------------------------------------------------------
//
// The display is retained-mode. Set what it should show, then call
// paintDisplay() every pass of loop(); it keeps the hardware in step.
//
//   setDisplayText("1203");          // fits, so it just sits there
//   setDisplayText("12.03");         // same four digits, decimal point lit
//   setDisplayText("Hello World");   // too long, so it scrolls
//
// A '.' attaches to the character before it instead of taking a digit of its
// own. Setting the same text again does nothing, so it is safe to call on
// every iteration without restarting a scroll in progress.

void setDisplayText(const char* text);
void setDisplayText(const char* text, int scrollSpeed);
void clearDisplay();
void paintDisplay();
bool isDisplayScrolling();

// --- Loading animation ---------------------------------------------------
//
// A comet that laps the outside edge of the whole display, drawn from raw
// segments rather than characters. While it is on it takes over the display;
// turning it off restores whatever setDisplayText() last set, untouched.
//
//   setLoading(true);        // spin
//   setLoading(true, 120);   // spin, slower
//   setLoading(false);       // back to normal content

void setLoading(bool on);
void setLoading(bool on, int frameMs);
bool isLoading();

int getDisplayDigitCount();

// --- Direct hardware access ----------------------------------------------
// Bypasses the display state above; useful for tests and one-off effects.

// Individual segments, for building patterns getCharacter() cannot express.
// OR them together to make a digit:  setSingleDigit(SEG_A | SEG_D, false);
//        aaaa
//       f    b
//       f    b
//        gggg
//       e    c
//       e    c
//        dddd   dot
const uint8_t SEG_A = 0b10000000; // top
const uint8_t SEG_B = 0b01000000; // top right
const uint8_t SEG_F = 0b00100000; // top left
const uint8_t SEG_G = 0b00010000; // middle
const uint8_t SEG_C = 0b00001000; // bottom right
const uint8_t SEG_D = 0b00000100; // bottom
const uint8_t SEG_E = 0b00000010; // bottom left

uint8_t getCharacter(char c);
void iShiftOut(uint8_t dataPin, uint8_t clockPin, uint8_t bitOrder, uint8_t val);
void setClock(uint8_t firstDigit, uint8_t secondDigit, uint8_t thirdDigit, uint8_t fourthDigit, bool firstDot, bool secondDot, bool thirdDot, bool fourthDot);
void setSingleDigit(uint8_t digit, bool dot);

#endif // SEVEN_SEGMENT_H
