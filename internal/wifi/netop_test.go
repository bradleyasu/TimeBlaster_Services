package wifi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradsheets/timeblaster/internal/system"
)

func TestLeavingSetupWaitsForAConnectionAttemptToFinish(t *testing.T) {
	// The Wi-Fi button cancels setup. Pressed while an attempt is under way,
	// bringing the old network up in the middle of joining the new one could
	// leave neither -- so the cancel waits its turn.
	s, runner, _ := newTestServer(t)
	ctx := context.Background()
	if err := s.EnterSetup(ctx); err != nil {
		t.Fatal(err)
	}

	joining := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var order []string
	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		line := c.String()
		mu.Lock()
		order = append(order, line)
		mu.Unlock()
		if strings.Contains(line, "connection up Home") {
			once.Do(func() { close(joining) })
			<-release
		}
		if isDeviceShow(c) {
			return []byte("GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:Home\nIP4.ADDRESS[1]:192.168.1.42/24\n"), nil
		}
		return nil, nil
	}

	connected := make(chan error, 1)
	go func() {
		_, err := s.Connect(ctx, "Home", "hunter22", false)
		connected <- err
	}()
	<-joining

	exited := make(chan error, 1)
	go func() { exited <- s.ExitSetup(ctx) }()
	select {
	case <-exited:
		t.Fatal("leaving setup ran in the middle of a connection attempt")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-connected; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := <-exited; err != nil {
		t.Fatalf("ExitSetup: %v", err)
	}
	// The attempt succeeded, so setup was already over: the cancel had
	// nothing left to undo, and must not have dragged the old network back.
	mu.Lock()
	defer mu.Unlock()
	for _, l := range order[indexOf(order, "connection up Home")+1:] {
		if strings.Contains(l, "connection up") {
			t.Errorf("a network was brought up after the join had succeeded: %s", l)
		}
	}
}

func indexOf(xs []string, sub string) int {
	for i, x := range xs {
		if strings.Contains(x, sub) {
			return i
		}
	}
	return -1
}

func TestStatusSaysJoiningWhileAnAttemptRuns(t *testing.T) {
	// Setup mode lasts until a join succeeds, so without this the setup page
	// cannot tell "joining" from "setup started again" -- and once called a
	// join that went on to succeed a failure.
	s, runner, _ := newTestServer(t)
	ctx := context.Background()
	if err := s.EnterSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Status(ctx).Joining {
		t.Fatal("joining before any attempt")
	}
	joining := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runner.OnRun = func(c system.RecordedCommand) ([]byte, error) {
		if strings.Contains(c.String(), "connection up Home") {
			once.Do(func() { close(joining) })
			<-release
		}
		if isDeviceShow(c) {
			return []byte("GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:Home\nIP4.ADDRESS[1]:192.168.1.42/24\n"), nil
		}
		return nil, nil
	}
	done := make(chan struct{})
	go func() { _, _ = s.Connect(ctx, "Home", "hunter22", false); close(done) }()
	<-joining
	if st := s.Status(ctx); !st.Joining || st.Mode != ModeSetup {
		t.Errorf("mid-attempt status %+v: want setup mode, joining", st)
	}
	close(release)
	<-done
	if s.Status(ctx).Joining {
		t.Error("still joining after the attempt finished")
	}
}
