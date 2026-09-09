package main

// Network fingerprinting.
//
// macOS withholds the SSID unless the caller holds Location Services
// authorization, which a launchd agent cannot obtain: there is no UI to present
// the prompt from. So instead of naming the network, we identify it by the
// Wi-Fi gateway: its IP, the subnet mask, and its MAC address.
//
// Every function here is a pure string to value transform. The daemon layer
// captures stdout from route/arp/ifconfig/ipconfig and passes it in, which
// means the tests feed fixtures through the same code that ships.

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

var (
	// ErrNoGateway means the interface has no scoped default route: either not
	// associated, or the -ifscope lookup came up empty.
	ErrNoGateway = errors.New("no gateway for interface")
	// ErrNoMAC means the gateway is not in the ARP cache yet, or its entry is
	// still "(incomplete)". Retrying on the next tick is the right response.
	ErrNoMAC = errors.New("no MAC address for gateway")
)

// Observation is one sampled view of the link.
type Observation struct {
	LinkUp bool
	FP     Fingerprint
}

// Fingerprint identifies a network. Vendor is a hint for labeling only and
// never part of the key, because it comes from DHCP and can change while the
// network stays the same.
type Fingerprint struct {
	GatewayIP  string
	Subnet     string // dotted quad netmask
	GatewayMAC string // lower-case colon form
	Vendor     string
}

// Key is the attribution key. The MAC is mandatory: thousands of networks share
// 203.0.113.1, so IP and subnet alone collide constantly.
func (f Fingerprint) Key() string {
	return f.GatewayIP + "|" + f.Subnet + "|" + f.GatewayMAC
}

func (f Fingerprint) String() string { return f.Key() }

// Complete reports whether all three key components are present.
func (f Fingerprint) Complete() bool {
	return f.GatewayIP != "" && f.Subnet != "" && f.GatewayMAC != ""
}

// Observe builds a fingerprint from captured command output. It returns
// ErrNoGateway or ErrNoMAC when the network cannot be identified, and the
// caller must drop that sample rather than attribute it somewhere else.
func Observe(routeOut, arpOut, ifconfigOut, summaryOut string) (Observation, error) {
	obs := Observation{LinkUp: parseLinkUp(ifconfigOut)}

	gw, err := parseGateway(routeOut)
	if err != nil {
		return obs, err
	}
	mac, err := parseGatewayMAC(arpOut)
	if err != nil {
		return obs, err
	}

	obs.FP = Fingerprint{
		GatewayIP:  gw,
		Subnet:     parseSubnet(ifconfigOut, summaryOut),
		GatewayMAC: mac,
		Vendor:     parseVendor(summaryOut),
	}
	return obs, nil
}

// parseGateway reads the gateway from `route -n get default -ifscope en0`:
//
//	 route to: default
//	  gateway: 203.0.113.1
//	interface: en0
//
// The -ifscope flag is what stops a system default route through utun7
// (Cloudflare WARP) from making us fingerprint the tunnel instead of the
// Wi-Fi network.
func parseGateway(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "gateway" {
			continue
		}
		val = strings.TrimSpace(val)
		if net.ParseIP(val) == nil {
			return "", fmt.Errorf("gateway %q is not an IP address", val)
		}
		return val, nil
	}
	return "", ErrNoGateway
}

// parseGatewayMAC reads the hardware address from `arp -n <gateway>`:
//
//	? (203.0.113.1) at 00:00:5e:00:53:0b on en0 ifscope [ethernet]
//	? (203.0.113.1) at (incomplete) on en0 ifscope [ethernet]
//
// net.ParseMAC normalizes to lower-case colon form, so one router seen twice
// yields one key rather than two.
func parseGatewayMAC(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		_, rest, ok := strings.Cut(line, " at ")
		if !ok {
			continue
		}
		mac, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
		hw, err := net.ParseMAC(padMACOctets(mac))
		if err != nil || len(hw) != 6 { // non-ethernet forms are not our gateway
			continue
		}
		return hw.String(), nil
	}
	return "", ErrNoMAC
}

// padMACOctets zero-pads single hex digits, turning 8:0:5e:2:53:7 into
// 08:00:5e:02:53:07. arp on macOS leaves the leading zero off, and
// net.ParseMAC rejects the short form, so a gateway holding any byte under
// 0x10 would fail to parse and lose every sample from that network.
func padMACOctets(s string) string {
	parts := strings.Split(s, ":")
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

// parseSubnet prefers the DHCP subnet mask over the interface netmask, since
// DHCP is what the network actually offered, and falls back to ifconfig when
// the summary carries none (static config, or a lease still forming).
func parseSubnet(ifconfigOut, summaryOut string) string {
	for line := range strings.SplitSeq(summaryOut, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "subnet_mask (ip):"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return parseIfconfigNetmask(ifconfigOut)
}

// parseIfconfigNetmask turns "netmask 0xffffff00" into "255.255.255.0".
func parseIfconfigNetmask(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		trimmed, ok := strings.CutPrefix(strings.TrimSpace(line), "inet ")
		if !ok {
			continue
		}
		wantNext := false
		for field := range strings.FieldsSeq(trimmed) {
			if wantNext {
				return hexMaskToQuad(field)
			}
			wantNext = field == "netmask"
		}
	}
	return ""
}

func hexMaskToQuad(s string) string {
	if strings.ContainsRune(s, '.') {
		if net.ParseIP(s) == nil {
			return ""
		}
		return s
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// parseVendor reads "vendor_specific: ANDROID_METERED", which is a hint that
// this network is a phone hotspot and therefore worth labeling as metered.
func parseVendor(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "vendor_specific:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseLinkUp reads "status: active" from ifconfig. A missing or unknown status
// counts as down, because a sample whose link we cannot confirm is up should
// not be attributed to anyone.
func parseLinkUp(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "status:"); ok {
			return strings.TrimSpace(v) == "active"
		}
	}
	return false
}
