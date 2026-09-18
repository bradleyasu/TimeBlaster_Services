package wifi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Manager is the networking interface the main daemon depends on. It is
// deliberately small: everything it can ask for is a well-defined operation with
// validated arguments, so the privilege boundary is easy to reason about.
type Manager interface {
	Status(ctx context.Context) (Status, error)
	EnterSetupMode(ctx context.Context) error
	ExitSetupMode(ctx context.Context) error
	Scan(ctx context.Context) ([]Network, error)
	Connect(ctx context.Context, ssid, passphrase string, hidden bool) (ConnectResult, error)
	// Available reports whether the helper is reachable.
	Available() bool
}

// ErrHelperUnavailable means the privileged helper could not be reached. The
// daemon surfaces it in /api/health rather than failing: losing Wi-Fi
// configuration is a degraded state, not a reason to stop ringing alarms.
var ErrHelperUnavailable = errors.New("wifi: the network helper is unavailable")

// Client talks to the helper over its unix socket.
type Client struct {
	socket  string
	timeout time.Duration
	log     *slog.Logger

	mu        sync.Mutex
	available bool
	lastErr   error
}

// NewClient creates a helper client.
func NewClient(socket string, timeout time.Duration, log *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{socket: socket, timeout: timeout, log: log}
}

// Available reports whether the most recent call reached the helper.
func (c *Client) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.available
}

// LastError returns the most recent helper error, for the health endpoint.
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastErr == nil {
		return ""
	}
	return c.lastErr.Error()
}

// Status reports the device's network state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	resp, err := c.call(ctx, Request{Op: OpStatus})
	if err != nil {
		return Status{}, err
	}
	if resp.Status == nil {
		return Status{}, errors.New("wifi: the helper returned no status")
	}
	return *resp.Status, nil
}

// EnterSetupMode raises the setup access point and captive portal.
func (c *Client) EnterSetupMode(ctx context.Context) error {
	_, err := c.call(ctx, Request{Op: OpEnterSetup})
	return err
}

// ExitSetupMode tears them down.
func (c *Client) ExitSetupMode(ctx context.Context) error {
	_, err := c.call(ctx, Request{Op: OpExitSetup})
	return err
}

// Scan lists visible networks.
func (c *Client) Scan(ctx context.Context) ([]Network, error) {
	resp, err := c.call(ctx, Request{Op: OpScan})
	if err != nil {
		return nil, err
	}
	return resp.Networks, nil
}

// Connect joins a network.
func (c *Client) Connect(ctx context.Context, ssid, passphrase string, hidden bool) (ConnectResult, error) {
	if err := ValidateCredentials(ssid, passphrase); err != nil {
		return ConnectResult{}, err
	}
	resp, err := c.call(ctx, Request{Op: OpConnect, SSID: ssid, Passphrase: passphrase, Hidden: hidden})
	if err != nil {
		return ConnectResult{}, err
	}
	if resp.Connect == nil {
		return ConnectResult{}, errors.New("wifi: the helper returned no connection result")
	}
	return *resp.Connect, nil
}

// Ping checks that the helper is alive.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, Request{Op: OpPing})
	return err
}

func (c *Client) call(ctx context.Context, req Request) (Response, error) {
	req.Version = ProtocolVersion

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		e := fmt.Errorf("%w: %s: %w", ErrHelperUnavailable, c.socket, err)
		c.setState(false, e)
		return Response{}, e
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	payload, err := encode(req)
	if err != nil {
		return Response{}, fmt.Errorf("wifi: encoding request: %w", err)
	}
	if _, err := conn.Write(payload); err != nil {
		e := fmt.Errorf("%w: writing request: %w", ErrHelperUnavailable, err)
		c.setState(false, e)
		return Response{}, e
	}

	// Responses are a single line, bounded so a confused helper cannot make the
	// daemon allocate without limit.
	r := bufio.NewReaderSize(conn, 256*1024)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		e := fmt.Errorf("%w: reading response: %w", ErrHelperUnavailable, err)
		c.setState(false, e)
		return Response{}, e
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		e := fmt.Errorf("wifi: decoding helper response: %w", err)
		c.setState(false, e)
		return Response{}, e
	}
	c.setState(true, nil)

	if !resp.OK {
		err := errors.New(resp.Error)
		if resp.Error == "" {
			err = errors.New("wifi: the helper reported an unspecified failure")
		}
		c.setState(true, err) // the helper answered; the operation failed
		return resp, err
	}
	return resp, nil
}

func (c *Client) setState(available bool, err error) {
	c.mu.Lock()
	c.available = available
	c.lastErr = err
	c.mu.Unlock()
}

var _ Manager = (*Client)(nil)
