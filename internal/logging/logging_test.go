package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"info": slog.LevelInfo, "": slog.LevelInfo, "nonsense": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError, " Error ": slog.LevelError,
	} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v want %v", in, got, want)
		}
	}
}

func TestTextFormatOmitsTimestampForJournald(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Level: "info", Format: "text", Output: &buf}).Info("alarm triggered", "alarm_id", 3)

	line := buf.String()
	if strings.Contains(line, "time=") {
		t.Errorf("journald adds its own timestamp; ours should be omitted: %s", line)
	}
	for _, want := range []string{"level=INFO", `msg="alarm triggered"`, "alarm_id=3"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
}

func TestIncludeTime(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Level: "info", Format: "text", Output: &buf, IncludeTime: true}).Info("hello")
	if !strings.Contains(buf.String(), "time=") {
		t.Errorf("expected a timestamp: %s", buf.String())
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Level: "info", Format: "json", Output: &buf}).Warn("nano disconnected", "device", "/dev/ttyACM0")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("not valid JSON: %v (%s)", err, buf.String())
	}
	if entry["msg"] != "nano disconnected" || entry["device"] != "/dev/ttyACM0" {
		t.Errorf("entry: %+v", entry)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Level: "warn", Format: "text", Output: &buf})

	log.Debug("pot report", "raw", 742)
	log.Info("channel change")
	if buf.Len() != 0 {
		t.Errorf("entries below the level leaked: %s", buf.String())
	}

	log.Warn("mpv restarted")
	if !strings.Contains(buf.String(), "mpv restarted") {
		t.Errorf("warn was filtered: %s", buf.String())
	}
}

func TestDiscardLoggerWritesNothing(t *testing.T) {
	// Discard is used by tests; it must not panic or write anywhere.
	log := Discard()
	log.Error("this goes nowhere")
	log.Info("neither does this")
}
