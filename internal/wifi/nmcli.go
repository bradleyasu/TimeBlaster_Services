package wifi

import (
	"sort"
	"strconv"
	"strings"
)

// NetworkManager is the networking stack on current Raspberry Pi OS (Debian 13
// "trixie"); the old wpa_supplicant/dhcpcd arrangement is gone. Everything here
// drives it through nmcli's terse, machine-readable mode (-t), which emits
// colon-separated fields with backslash escaping.
//
// Parsing is split out from execution so the exact command lines and their
// output handling are unit-tested without NetworkManager present.

// SetupConnectionName is the NetworkManager profile used for the setup access
// point. Keeping it separate from any user profile means bringing setup mode up
// and down never touches the credentials for the user's real network.
const SetupConnectionName = "timeblaster-setup"

// ScanArgs builds the command that lists visible networks.
//
// --rescan yes forces a fresh scan rather than returning a cached list, which
// matters because the user is standing in front of the device having just moved
// it to a new location.
func ScanArgs(iface string) []string {
	return []string{
		"-t", "-f", "SSID,SIGNAL,SECURITY,IN-USE",
		"device", "wifi", "list", "ifname", iface, "--rescan", "yes",
	}
}

// ParseScan parses `nmcli -t -f SSID,SIGNAL,SECURITY,IN-USE device wifi list`.
//
// Duplicate SSIDs (a mesh network with several access points) are collapsed to
// the strongest, and hidden networks with an empty SSID are dropped, because
// neither is something a user can act on in the portal.
func ParseScan(out string) []Network {
	best := map[string]Network{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := splitTerse(line)
		if len(fields) < 2 {
			continue
		}
		ssid := fields[0]
		if ssid == "" || ssid == "--" {
			continue
		}
		signal, _ := strconv.Atoi(strings.TrimSpace(fields[1]))

		n := Network{SSID: ssid, Signal: signal}
		if len(fields) > 2 {
			sec := strings.TrimSpace(fields[2])
			if sec != "" && sec != "--" {
				n.Security = sec
			}
		}
		if len(fields) > 3 {
			n.InUse = strings.TrimSpace(fields[3]) == "*"
		}

		if prev, ok := best[ssid]; !ok || n.Signal > prev.Signal {
			// Preserve the in-use marker even if a weaker duplicate carried it.
			n.InUse = n.InUse || prev.InUse
			best[ssid] = n
		} else if n.InUse {
			prev.InUse = true
			best[ssid] = prev
		}
	}

	out2 := make([]Network, 0, len(best))
	for _, n := range best {
		out2 = append(out2, n)
	}
	// Strongest first, then alphabetically, so the list the user sees is stable
	// between refreshes rather than reshuffling on every scan.
	sort.Slice(out2, func(i, j int) bool {
		if out2[i].Signal != out2[j].Signal {
			return out2[i].Signal > out2[j].Signal
		}
		return out2[i].SSID < out2[j].SSID
	})
	return out2
}

// DeviceShowArgs builds the command that reports one interface's state.
func DeviceShowArgs(iface string) []string {
	return []string{
		"-t", "-f", "GENERAL.STATE,GENERAL.CONNECTION,IP4.ADDRESS",
		"device", "show", iface,
	}
}

// DeviceState is the parsed result of `nmcli device show`.
type DeviceState struct {
	// State is NetworkManager's numeric state description, e.g. "100 (connected)".
	State string
	// Connected is true when the interface is fully connected.
	Connected bool
	// Connection is the active profile name, which for a Wi-Fi client is the SSID.
	Connection string
	// IPv4 is the first address, e.g. "192.168.1.42/24".
	IPv4 string
}

// ParseDeviceShow parses `nmcli -t device show <iface>` output.
func ParseDeviceShow(out string) DeviceState {
	var st DeviceState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = unescapeTerse(strings.TrimSpace(value))

		switch {
		case key == "GENERAL.STATE":
			st.State = value
			// NetworkManager reports "100 (connected)"; anything below 100 is a
			// transitional or disconnected state.
			st.Connected = strings.HasPrefix(value, "100")
		case key == "GENERAL.CONNECTION":
			if value != "--" {
				st.Connection = value
			}
		case strings.HasPrefix(key, "IP4.ADDRESS") && st.IPv4 == "":
			if value != "--" {
				st.IPv4 = value
			}
		}
	}
	return st
}

// ConnectArgs builds the command that joins a network.
//
// nmcli creates or updates a profile named after the SSID. Crucially, it does
// not remove the profile for any other network: the previous known-good
// credentials survive a failed attempt, which is what makes rollback possible.
func ConnectArgs(iface, ssid, passphrase string, hidden bool) []string {
	args := []string{"device", "wifi", "connect", ssid, "ifname", iface}
	if passphrase != "" {
		args = append(args, "password", passphrase)
	}
	if hidden {
		args = append(args, "hidden", "yes")
	}
	return args
}

// APUpArgs builds the commands that raise the setup access point.
//
// ipv4.method shared makes NetworkManager run its own dnsmasq for DHCP and DNS
// on the interface, which is exactly what a captive portal needs and avoids
// hand-managing a second DHCP server.
func APUpArgs(iface, ssid, passphrase, address string) [][]string {
	cmds := [][]string{
		// Delete any leftover profile first so a changed SSID or passphrase in the
		// config file actually takes effect. Failure is ignored by the caller: the
		// usual case is that there is nothing to delete.
		{"connection", "delete", SetupConnectionName},
		{"connection", "add",
			"type", "wifi",
			"ifname", iface,
			"con-name", SetupConnectionName,
			"autoconnect", "no", // only ever up while the user asked for setup mode
			"ssid", ssid,
		},
		{"connection", "modify", SetupConnectionName,
			"802-11-wireless.mode", "ap",
			"802-11-wireless.band", "bg",
			"ipv4.method", "shared",
			"ipv4.addresses", address,
		},
	}
	if passphrase != "" {
		cmds = append(cmds, []string{"connection", "modify", SetupConnectionName,
			"wifi-sec.key-mgmt", "wpa-psk",
			"wifi-sec.psk", passphrase,
		})
	}
	return append(cmds, []string{"connection", "up", SetupConnectionName})
}

// APDownArgs builds the commands that tear the access point down.
func APDownArgs() [][]string {
	return [][]string{
		{"connection", "down", SetupConnectionName},
		{"connection", "delete", SetupConnectionName},
	}
}

// ReconnectArgs builds the command that brings the interface back to its normal
// automatic behaviour after setup mode ends.
func ReconnectArgs(iface string) []string {
	return []string{"device", "connect", iface}
}

// ActiveConnectionArgs lists active connections so the current network can be
// remembered before setup mode takes the interface over.
func ActiveConnectionArgs() []string {
	return []string{"-t", "-f", "NAME,DEVICE,TYPE", "connection", "show", "--active"}
}

// ActiveConnection is one row of `nmcli connection show --active`.
type ActiveConnection struct {
	Name   string
	Device string
	Type   string
}

// ParseActiveConnections parses `nmcli -t -f NAME,DEVICE,TYPE connection show --active`.
func ParseActiveConnections(out string) []ActiveConnection {
	var conns []ActiveConnection
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 3 {
			continue
		}
		conns = append(conns, ActiveConnection{Name: f[0], Device: f[1], Type: f[2]})
	}
	return conns
}

// WirelessConnectionFor returns the active wireless profile on an interface,
// which is the connection to restore if a new one fails to validate.
func WirelessConnectionFor(conns []ActiveConnection, iface string) (string, bool) {
	for _, c := range conns {
		if c.Device != iface {
			continue
		}
		if !strings.Contains(strings.ToLower(c.Type), "wireless") &&
			!strings.Contains(strings.ToLower(c.Type), "wifi") {
			continue
		}
		if c.Name == SetupConnectionName {
			continue // our own access point is never the network to go back to
		}
		return c.Name, true
	}
	return "", false
}

// splitTerse splits an nmcli terse line on unescaped colons.
//
// nmcli escapes literal colons and backslashes inside values, so an SSID
// containing a colon — which is legal and does occur — must not be mistaken for
// a field separator.
func splitTerse(line string) []string {
	var (
		fields []string
		cur    strings.Builder
	)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '\\':
			if i+1 < len(line) {
				i++
				cur.WriteByte(line[i])
			}
		case ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(fields, cur.String())
}

// unescapeTerse removes nmcli's backslash escaping from a single value.
func unescapeTerse(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
