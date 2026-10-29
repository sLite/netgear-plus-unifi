package identity

import (
	"net"
	"testing"
)

func TestDeriveMACIsStableLocallyAdministeredUnicast(t *testing.T) {
	a, b := DeriveMAC("gs308ev4"), DeriveMAC("gs308ev4")
	if a.String() != b.String() {
		t.Fatal("derivation must be deterministic")
	}
	if a.String()[:9] != "02:4e:47:" {
		t.Errorf("missing project prefix: %s", a)
	}
	if a[0]&0x02 == 0 || a[0]&0x01 != 0 {
		t.Errorf("%s must be locally administered unicast", a)
	}
	if DeriveMAC("gs116ev2").String() == a.String() {
		t.Error("different switches must get different MACs")
	}
}

func TestResolveMAC(t *testing.T) {
	for _, auto := range []string{"", "auto", "AUTO"} {
		if got, err := ResolveMAC("x", auto); err != nil || got.String() != DeriveMAC("x").String() {
			t.Errorf("ResolveMAC(%q) = %v, %v", auto, got, err)
		}
	}
	if got, err := ResolveMAC("x", "06:00:00:00:00:01"); err != nil || got.String() != "06:00:00:00:00:01" {
		t.Errorf("explicit LAA rejected: %v %v", got, err)
	}
	for _, bad := range []string{"00:27:22:00:00:01", "03:00:00:00:00:01", "nonsense", "02:00:00:00:00:00:00:01"} {
		if _, err := ResolveMAC("x", bad); err == nil {
			t.Errorf("ResolveMAC accepted %q", bad)
		}
	}
	if got := Serial(net.HardwareAddr{0x02, 0x4e, 0x47, 0xab, 0xcd, 0xef}); got != "024E47ABCDEF" {
		t.Errorf("serial = %q, want uppercase MAC without separators", got)
	}
}

func TestPhysicalMode(t *testing.T) {
	if !IsPhysical("physical") || !IsPhysical(" Physical ") || IsPhysical("auto") {
		t.Error("IsPhysical misclassifies")
	}
	if _, err := ResolveMAC("x", "physical"); err == nil {
		t.Error("physical MACs cannot be resolved from config")
	}
	if hw, err := ParseHardwareMAC("28:94:01:8F:FF:71"); err != nil || hw.String() != "28:94:01:8f:ff:71" {
		t.Errorf("ParseHardwareMAC = %v, %v", hw, err)
	}
	for _, bad := range []string{"", "nonsense", "01:00:5e:00:00:01", "00:00:00:00:00:00"} {
		if _, err := ParseHardwareMAC(bad); err == nil {
			t.Errorf("ParseHardwareMAC accepted %q", bad)
		}
	}
}
