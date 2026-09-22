# Protocol test fixtures

`firmware-frames.bin` is real output from the Arduino firmware's encoder
(`firmware/timeblaster-nano/src/Protocol.cpp`), captured by compiling it for the
host and running it against the Arduino stub.

It exists so that `TestDecodesRealFirmwareOutput` pins the two independent
implementations of the TB1 wire format against each other. A change to the
framing, the escaping or the CRC on either side fails that test rather than
appearing as a Timeblaster that has stopped responding to its own knobs.

To regenerate after a deliberate protocol change:

```bash
make -C firmware/timeblaster-nano fixture
```
