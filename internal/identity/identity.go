// Package identity derives the virtual UniFi identity of an emulated switch.
// By default ("auto") virtual identities are locally administered addresses,
// which no vendor burns into hardware, under a fixed project prefix so they
// are recognisable in UniFi. PhysicalMode instead reuses the NETGEAR's own
// MAC, so that the gateway sees the device's identity on the wire and UniFi
// can place it in the topology.
package identity

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
)

// Prefix is the first three octets of every derived virtual MAC: 0x02 marks a
// locally administered unicast address, 0x4e 0x47 spell "NG".
var Prefix = [3]byte{0x02, 0x4e, 0x47}

// DeriveMAC returns the stable virtual MAC for a switch id. The same id
// always yields the same MAC, so the identity survives reinstalls without
// any stored state.
func DeriveMAC(switchID string) net.HardwareAddr {
	sum := sha256.Sum256([]byte("netgear-plus-unifi/" + switchID))
	return net.HardwareAddr{Prefix[0], Prefix[1], Prefix[2], sum[0], sum[1], sum[2]}
}

// PhysicalMode is the unifi.mac value that makes the virtual device use the
// NETGEAR's own MAC, so the rest of the network (e.g. the gateway's
// forwarding table) sees the device's identity on the wire.
const PhysicalMode = "physical"

// IsPhysical reports whether a configured unifi.mac selects PhysicalMode.
func IsPhysical(configured string) bool {
	return strings.EqualFold(strings.TrimSpace(configured), PhysicalMode)
}

// ParseHardwareMAC validates a MAC reported by the switch for PhysicalMode.
func ParseHardwareMAC(reported string) (net.HardwareAddr, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(reported))
	if err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("switch reported no usable MAC (%q)", reported)
	}
	if hw[0]&0x01 != 0 || hw.String() == "00:00:00:00:00:00" {
		return nil, fmt.Errorf("switch reported MAC %s, which is not a unicast address", hw)
	}
	return hw, nil
}

// ResolveMAC returns the configured MAC, or the derived one for "" / "auto".
// PhysicalMode is not resolvable here: that MAC comes from the switch.
// A configured MAC must be a locally administered unicast address so it
// cannot collide with a burned-in address of real equipment.
func ResolveMAC(switchID, configured string) (net.HardwareAddr, error) {
	if configured == "" || strings.EqualFold(configured, "auto") {
		return DeriveMAC(switchID), nil
	}
	if IsPhysical(configured) {
		return nil, fmt.Errorf("virtual MAC %q is learned from the switch at runtime", configured)
	}
	hw, err := net.ParseMAC(configured)
	if err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("virtual MAC %q is not a 6-byte MAC", configured)
	}
	if hw[0]&0x01 != 0 {
		return nil, fmt.Errorf("virtual MAC %s is multicast", hw)
	}
	if hw[0]&0x02 == 0 {
		return nil, fmt.Errorf("virtual MAC %s is universally administered; use a locally administered address (second hex digit 2, 6, A or E) or \"auto\"", hw)
	}
	return hw, nil
}

// Serial returns the serial number reported for a virtual MAC, in the
// uppercase separator-less form UniFi devices use.
func Serial(mac net.HardwareAddr) string {
	return strings.ToUpper(strings.ReplaceAll(mac.String(), ":", ""))
}
