package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

type fakeFirmware struct {
	mu      sync.Mutex
	status  FirmwareStatus
	err     error
	updates int
}

func (f *fakeFirmware) FirmwareStatus() FirmwareStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeFirmware) UpdateFirmware() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if f.err != nil {
		return f.err
	}
	f.status.Updating, f.status.UpdateAvailable = true, false
	return nil
}

func TestFirmwareStatusEndpoint(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/firmware", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var st FirmwareStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Running != "0123abc" || st.Available != "abc1234" || !st.UpdateAvailable {
		t.Errorf("got %+v", st)
	}
}

func TestFirmwareUpdateEndpointStartsAnUpdate(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/firmware/update", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if f.firmware.updates != 1 {
		t.Errorf("UpdateFirmware called %d times", f.firmware.updates)
	}
	var st FirmwareStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Updating {
		t.Error("the response does not show the update running")
	}
}

func TestFirmwareUpdateEndpointReportsARefusalAsAConflict(t *testing.T) {
	f := newFixture(t)
	f.firmware.err = fmt.Errorf("%w: an alarm is ringing", ErrFirmwareRefused)
	rec := f.do(t, http.MethodPost, "/api/firmware/update", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("status %d, want 409: %s", rec.Code, rec.Body)
	}
}

func TestFirmwareUpdateIsNotAGet(t *testing.T) {
	// A link, a prefetch or a crawler must never be able to flash the Nano.
	f := newFixture(t)
	if rec := f.do(t, http.MethodGet, "/api/firmware/update", ""); rec.Code == http.StatusAccepted {
		t.Error("a GET started a firmware update")
	}
	if f.firmware.updates != 0 {
		t.Errorf("UpdateFirmware called %d times by a GET", f.firmware.updates)
	}
}
