package system

import (
	"context"
	"errors"
	"testing"
)

func TestFakeRunnerRecordsCalls(t *testing.T) {
	f := NewFakeRunner()
	f.Outputs["amixer"] = []byte("ok")

	out, err := f.Run(context.Background(), "amixer", "-c", "2", "sset", "PCM", "40%")
	if err != nil || string(out) != "ok" {
		t.Fatalf("Run: %q, %v", out, err)
	}
	last, ok := f.LastCall()
	if !ok {
		t.Fatal("no call recorded")
	}
	if got := last.String(); got != "amixer -c 2 sset PCM 40%" {
		t.Errorf("recorded: %q", got)
	}
	f.Reset()
	if _, ok := f.LastCall(); ok {
		t.Error("Reset did not clear calls")
	}
}

func TestFakeRunnerReturnsConfiguredError(t *testing.T) {
	f := NewFakeRunner()
	want := errors.New("boom")
	f.Errors["nmcli"] = want
	if _, err := f.Run(context.Background(), "nmcli", "dev"); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

func TestExecRunnerCapturesStderrOnFailure(t *testing.T) {
	r := ExecRunner{}
	_, err := r.Run(context.Background(), "sh", "-c", "echo bad things >&2; exit 3")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !contains(got, "bad things") {
		t.Errorf("stderr not surfaced: %v", err)
	}
}

func TestExecRunnerReturnsStdout(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "printf hello")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("got %q", out)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
