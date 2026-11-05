package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// arrivingOn makes a request look as though it reached the server on addr.
func arrivingOn(r *http.Request, addr string) *http.Request {
	a, _ := net.ResolveTCPAddr("tcp", addr)
	return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(a)))
}

func TestTheAppOverTheSetupNetworkHandsOverToTheSetupPage(t *testing.T) {
	// In setup mode the NFC tag's timeblaster.local:8080 resolves to the setup
	// network. There is no app to offer there -- the Timeblaster is not on a
	// network yet -- so send the visitor to the setup page.
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, arrivingOn(httptest.NewRequest(http.MethodGet, "/", nil), "10.42.0.1:8080"))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "http://10.42.0.1/" {
		t.Errorf("got %d to %q, want a redirect to the setup page", rec.Code, rec.Header().Get("Location"))
	}

	// The API is left alone: the setup page asks it whether setup is over.
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, arrivingOn(httptest.NewRequest(http.MethodGet, "/api/wifi/status", nil), "10.42.0.1:8080"))
	if rec.Code == http.StatusFound {
		t.Error("the API was redirected away over the setup network")
	}
}

func TestTheAppOnTheHomeNetworkIsServedAsNormal(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, arrivingOn(httptest.NewRequest(http.MethodGet, "/", nil), "192.168.1.50:8080"))
	if rec.Code == http.StatusFound {
		t.Errorf("the app was redirected on the home network, to %q", rec.Header().Get("Location"))
	}
}
