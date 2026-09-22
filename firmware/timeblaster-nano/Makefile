# Host-side tests for the Timeblaster firmware.
#
# The Arduino API is stubbed (test/host/Arduino.h), so the display's segment
# patterns, polarity and digit order can be asserted with nothing but a C++
# compiler. Flashing still needs PlatformIO; see README.md.

CXX      ?= c++
CXXFLAGS := -std=c++17 -Wall -O1 -I test/host -I src

TEST_SRCS := src/SevenSegment.cpp src/Display.cpp src/Inputs.cpp src/Protocol.cpp \
             test/host/Arduino.cpp test/host/test_firmware.cpp

FIXTURE := ../../internal/protocol/testdata/firmware-frames.bin

.PHONY: test fixture clean
test: ## Build and run the host tests
	@$(CXX) $(CXXFLAGS) $(TEST_SRCS) -o /tmp/timeblaster-firmware-test
	@/tmp/timeblaster-firmware-test

fixture: ## Regenerate the frames the Go interop tests decode
	@$(CXX) $(CXXFLAGS) -w src/Protocol.cpp test/host/Arduino.cpp test/host/emit_fixture.cpp \
		-o /tmp/timeblaster-emit-fixture
	@/tmp/timeblaster-emit-fixture > $(FIXTURE)
	@printf 'wrote %s (%s bytes)\n' "$(FIXTURE)" "$$(wc -c < $(FIXTURE) | tr -d ' ')"
	@printf 'now run: go test ./internal/protocol/\n'

clean:
	@rm -f /tmp/timeblaster-firmware-test /tmp/timeblaster-emit-fixture
