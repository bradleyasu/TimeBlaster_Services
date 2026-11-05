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
