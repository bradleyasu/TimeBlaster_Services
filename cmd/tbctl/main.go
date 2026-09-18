// Command tbctl is the Timeblaster operator's command-line tool.
//
// It talks to a running timeblasterd over its HTTP API rather than touching the
// database or the hardware directly, so anything it can do is something the
// companion app can do too, and nothing it does can corrupt state behind the
// daemon's back.
//
// It exists because the most common questions on a headless appliance — "is it
// healthy", "what are my alarms", "which serial ports exist" — should be one
// command over SSH rather than a curl incantation.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bradsheets/timeblaster/internal/config"
	"github.com/bradsheets/timeblaster/internal/serialport"
)

var version = "dev"

const usage = `tbctl — Timeblaster control

Usage:
  tbctl [flags] <command> [arguments]

Commands:
  health                 component health summary
  state                  the full state document
  alarms                 list alarms
  next                   the next scheduled alarm
  dismiss                dismiss the ringing alarm
  snooze                 snooze the ringing alarm
  trigger <id>           start an alarm now (for testing)
  sounds                 list available alarm sounds
  play <sound-id>        play a sound on the alarm speaker
  stop                   stop alarm speaker playback
  volume                 show the alarm volume
  channels               list ErsatzTV channels
  channel <number>       select a channel
  channel off            show the standby image
  wifi                   network status
  wifi-setup             enter Wi-Fi setup mode (same as holding the button)
  ports                  list serial ports visible to this machine
  version                print the version

Flags:
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tbctl: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr       = flag.String("addr", "", "daemon address (default: from the config file, else http://127.0.0.1:8080)")
		configPath = flag.String("config", config.DefaultPath, "path to the configuration file")
		timeout    = flag.Duration("timeout", 30*time.Second, "request timeout")
		rawJSON    = flag.Bool("json", false, "print raw JSON instead of a summary")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return errors.New("no command given")
	}

	c := &client{base: resolveAddr(*addr, *configPath), timeout: *timeout, raw: *rawJSON}

	switch args[0] {
	case "version":
		fmt.Println("tbctl", version)
		return nil
	case "ports":
		return listPorts()
	case "health":
		return c.health()
	case "state":
		return c.get("/api/state")
	case "alarms":
		return c.alarms()
	case "next":
		return c.get("/api/alarm/active")
	case "dismiss":
		return c.post("/api/alarm/dismiss", nil)
	case "snooze":
		return c.post("/api/alarm/snooze", nil)
	case "trigger":
		if len(args) < 2 {
			return errors.New("trigger needs an alarm id")
		}
		return c.post("/api/alarms/"+args[1]+"/trigger", nil)
	case "sounds":
		return c.sounds()
	case "play":
		if len(args) < 2 {
			return errors.New("play needs a sound id")
		}
		return c.post("/api/sounds/"+args[1]+"/preview", nil)
	case "stop":
		return c.post("/api/sounds/preview/stop", nil)
	case "volume":
		return c.get("/api/volume")
	case "channels":
		return c.channels()
	case "channel":
		if len(args) < 2 {
			return errors.New("channel needs a number, or \"off\"")
		}
		if args[1] == "off" {
			return c.post("/api/channels/clear", nil)
		}
		return c.post("/api/channels/select", map[string]string{"number": args[1]})
	case "wifi":
		return c.get("/api/wifi/status")
	case "wifi-setup":
		fmt.Fprintln(os.Stderr, "Entering Wi-Fi setup mode. The device will leave the current network.")
		return c.post("/api/wifi/setup", nil)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// resolveAddr works out where the daemon is listening, preferring the config
// file so that a non-default port is picked up without a flag.
func resolveAddr(explicit, configPath string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	listen := cfg.Web.ListenAddress
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return "http://" + listen
}

type client struct {
	base    string
	timeout time.Duration
	raw     bool
}

func (c *client) do(method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: c.timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach timeblasterd at %s: %w\n"+
			"Is the service running? Try: systemctl status timeblaster.service", c.base, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			if e.Detail != "" {
				return data, fmt.Errorf("%s (%s)", e.Error, e.Detail)
			}
			return data, errors.New(e.Error)
		}
		return data, fmt.Errorf("HTTP %s", resp.Status)
	}
	return data, nil
}

func (c *client) get(path string) error {
	data, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return printJSON(data)
}

func (c *client) post(path string, body any) error {
	data, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		fmt.Println("ok")
		return nil
	}
	return printJSON(data)
}

func (c *client) health() error {
	data, err := c.do(http.MethodGet, "/api/health", nil)
	if err != nil {
		return err
	}
	if c.raw {
		return printJSON(data)
	}

	var h struct {
		Status     string            `json:"status"`
		Version    string            `json:"version"`
		Uptime     string            `json:"uptime"`
		Components map[string]string `json:"components"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return printJSON(data)
	}

	fmt.Printf("status   %s\nversion  %s\nuptime   %s\n\n", strings.ToUpper(h.Status), h.Version, h.Uptime)
	names := sortedKeys(h.Components)
	for _, k := range names {
		fmt.Printf("  %-14s %s\n", k, h.Components[k])
	}
	if h.Status != "ok" {
		// A non-zero exit makes tbctl usable from a monitoring script.
		defer os.Exit(2)
	}
	return nil
}

func (c *client) alarms() error {
	data, err := c.do(http.MethodGet, "/api/alarms", nil)
	if err != nil {
		return err
	}
	if c.raw {
		return printJSON(data)
	}

	var body struct {
		Alarms []struct {
			ID             int64      `json:"id"`
			Label          string     `json:"label"`
			Hour           int        `json:"hour"`
			Minute         int        `json:"minute"`
			Enabled        bool       `json:"enabled"`
			RepeatLabel    string     `json:"repeat_label"`
			SoundID        string     `json:"sound_id"`
			NextOccurrence *time.Time `json:"next_occurrence"`
		} `json:"alarms"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return printJSON(data)
	}
	if len(body.Alarms) == 0 {
		fmt.Println("no alarms configured")
		return nil
	}

	fmt.Printf("%-4s %-6s %-4s %-14s %-10s %s\n", "ID", "TIME", "ON", "REPEAT", "SOUND", "NEXT")
	for _, a := range body.Alarms {
		state := "no"
		if a.Enabled {
			state = "yes"
		}
		next := "—"
		if a.NextOccurrence != nil {
			next = a.NextOccurrence.Local().Format("Mon 15:04")
		}
		fmt.Printf("%-4d %02d:%02d  %-4s %-14s %-10s %s%s\n",
			a.ID, a.Hour, a.Minute, state, a.RepeatLabel, a.SoundID, next,
			labelSuffix(a.Label))
	}
	return nil
}

func (c *client) sounds() error {
	data, err := c.do(http.MethodGet, "/api/sounds", nil)
	if err != nil {
		return err
	}
	if c.raw {
		return printJSON(data)
	}
	var body struct {
		Sounds []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
			Fallback  bool   `json:"fallback"`
		} `json:"sounds"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return printJSON(data)
	}
	for _, s := range body.Sounds {
		note := ""
		if s.Fallback {
			note = "  (built-in fallback)"
		}
		fmt.Printf("%-24s %-22s %8d bytes%s\n", s.ID, s.Name, s.SizeBytes, note)
	}
	return nil
}

func (c *client) channels() error {
	data, err := c.do(http.MethodGet, "/api/channels", nil)
	if err != nil {
		return err
	}
	if c.raw {
		return printJSON(data)
	}
	var body struct {
		Channels []struct {
			Number string `json:"number"`
			Name   string `json:"name"`
		} `json:"channels"`
		Current *struct {
			Number string `json:"number"`
		} `json:"current"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return printJSON(data)
	}
	if len(body.Channels) == 0 {
		fmt.Println("no channels; is ErsatzTV running and configured?")
		return nil
	}
	for _, ch := range body.Channels {
		marker := "  "
		if body.Current != nil && body.Current.Number == ch.Number {
			marker = "> "
		}
		fmt.Printf("%s%-6s %s\n", marker, ch.Number, ch.Name)
	}
	return nil
}

func listPorts() error {
	ports, err := serialport.ListPorts()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		fmt.Println("no serial ports found")
		return nil
	}
	for _, p := range ports {
		fmt.Println(p)
	}
	return nil
}

func printJSON(data []byte) error {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		fmt.Println(string(data))
		return nil
	}
	fmt.Println(out.String())
	return nil
}

func labelSuffix(label string) string {
	if label == "" {
		return ""
	}
	return "  " + label
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
