package provision_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	. "netgear-plus-unifi/internal/provision"
)

const (
	U = Untagged
	T = Tagged
	E = Excluded
)

var caps = Capabilities{
	Speeds:   []Speed{SpeedAuto, SpeedDisabled, Speed10Half, Speed10Full, Speed100Half, Speed100Full},
	MaxVLANs: 64,
}

func all(ports int, m Member) Members {
	out := Members{}
	for p := 1; p <= ports; p++ {
		out[p] = m
	}
	return out
}

func wantOps(t *testing.T, got Plan, want ...Operation) {
	t.Helper()
	if len(want) == 0 && len(got.Operations) == 0 {
		return
	}
	if !reflect.DeepEqual(got.Operations, want) {
		t.Errorf("operations:\n got %v\nwant %v", got.Describe(), Plan{Operations: want}.Describe())
	}
}

func TestParseCapturedControllerConfig(t *testing.T) {
	raw, err := os.ReadFile("../../docs/captures/system_cfg-switch-section.txt")
	if err != nil {
		t.Fatal(err)
	}
	parsed := ParseUniFi(SwitchSection(string(raw)), 16)
	cfg := parsed.Config
	if len(cfg.VLANs) != 3 || cfg.VLANs[1] == nil || cfg.VLANs[20] == nil || cfg.VLANs[30] == nil {
		t.Fatalf("VLANs = %v, want 1, 20, 30", cfg.VLANs)
	}
	check := func(port int, want ...Member) {
		t.Helper()
		got := []Member{cfg.Member(1, port), cfg.Member(20, port), cfg.Member(30, port)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("port %d memberships (1, 20, 30) = %v, want %v", port, got, want)
		}
	}
	check(3, E, U, E) // access port in VLAN 20
	check(4, U, T, E) // trunk: native default, tagged 20, excluded 30
	check(1, U, T, T) // UniFi default "allow all"
	if cfg.PVIDs[3] != 20 {
		t.Errorf("PVID port 3 = %d", cfg.PVIDs[3])
	}
	if cfg.Ports[5].Speed != SpeedDisabled || cfg.Ports[6].Speed != Speed100Full {
		t.Errorf("speeds: %v %v", cfg.Ports[5], cfg.Ports[6])
	}
	if cfg.Ports[3].Name != SetName("access-vlan20") || cfg.Ports[1].Name != SetName("") {
		t.Errorf("names: %v %v (\"Port 1\" is UniFi's default: unnamed)", cfg.Ports[3].Name, cfg.Ports[1].Name)
	}
	if len(parsed.Warnings) != 0 {
		t.Errorf("warnings: %v", parsed.Warnings)
	}
}

func TestUnsupportedForcedSpeedIsAWarning(t *testing.T) {
	parsed := ParseUniFi("switch.port.2.autoneg=disabled\nswitch.port.2.speed=2500\nswitch.port.2.duplex=enabled", 4)
	if parsed.Config.Ports[2].Speed != SpeedAuto || len(parsed.Warnings) != 1 || !strings.Contains(parsed.Warnings[0], "2500") {
		t.Errorf("got %v, warnings %v", parsed.Config.Ports[2], parsed.Warnings)
	}
}

func TestAccessPortPlanIsOrdered(t *testing.T) {
	desired := FactoryDefault(8)
	desired.VLANs[3901] = Members{4: U}
	delete(desired.VLANs[1], 4)
	desired.PVIDs[4] = 3901
	wantOps(t, MakePlan(desired, FactoryDefault(8), 8, caps),
		CreateVLAN{3901, Members{4: U}}, SetPVID{4, 3901}, SetMembers{1, Members{4: E}})
}

func TestEveryPortIsManaged(t *testing.T) {
	desired := FactoryDefault(8)
	desired.VLANs[3901] = all(8, T) // UniFi "allow all" on every port
	desired.Ports[2] = PortSettings{Speed: SpeedDisabled}
	wantOps(t, MakePlan(desired, FactoryDefault(8), 8, caps),
		CreateVLAN{3901, all(8, T)}, SetPort{Port: 2, Speed: SpeedDisabled})
}

func TestVLANsUnknownToUniFiOrLeftEmptyAreDeleted(t *testing.T) {
	actual := FactoryDefault(8)
	actual.VLANs[1234] = Members{8: T} // created in the NETGEAR web UI
	actual.VLANs[3901] = Members{4: T}
	desired := FactoryDefault(8)
	desired.VLANs[3901] = Members{} // network exists in UniFi but no port uses it
	wantOps(t, MakePlan(desired, actual, 8, caps), DeleteVLAN{1234}, DeleteVLAN{3901})
}

func TestDefaultAndManagementVLANAreNeverDeleted(t *testing.T) {
	actual := FactoryDefault(8)
	actual.VLANs[10] = Members{8: T}
	actual.ManagementVLAN = 10
	desired := FactoryDefault(8)
	desired.VLANs[1] = Members{}
	desired.VLANs[3901] = all(8, U)
	for p := 1; p <= 8; p++ {
		desired.PVIDs[p] = 3901
	}
	plan := MakePlan(desired, actual, 8, caps)
	for _, op := range plan.Operations {
		if op == (DeleteVLAN{1}) || op == (DeleteVLAN{10}) {
			t.Errorf("protected VLAN deleted: %v", op)
		}
	}
	if len(plan.Warnings) != 2 {
		t.Errorf("warnings = %v, want two", plan.Warnings)
	}
}

func TestTrunkWithCustomTaggedVLANs(t *testing.T) {
	actual := FactoryDefault(8)
	actual.VLANs[3901] = Members{4: T}
	actual.VLANs[3902] = Members{4: T, 5: T}
	desired := FactoryDefault(8)
	desired.VLANs[3901] = Members{}
	desired.VLANs[3902] = Members{4: T, 5: T}
	desired.VLANs[3903] = Members{4: T}
	wantOps(t, MakePlan(desired, actual, 8, caps), CreateVLAN{3903, Members{4: T}}, DeleteVLAN{3901})
}

func TestPVIDNeedsMembership(t *testing.T) {
	desired := FactoryDefault(8)
	desired.PVIDs[4] = 3901 // but port 4 is not a member of 3901
	plan := MakePlan(desired, FactoryDefault(8), 8, caps)
	wantOps(t, plan)
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "not a member") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestPortSettings(t *testing.T) {
	desired := FactoryDefault(8)
	desired.Ports[4] = PortSettings{Speed: Speed100Full, Name: SetName("lab")}
	wantOps(t, MakePlan(desired, FactoryDefault(8), 8, caps), SetPort{4, Speed100Full, "lab"})

	desired.Ports[4] = PortSettings{Speed: Speed10Half}
	gs308 := Capabilities{Speeds: []Speed{SpeedAuto, SpeedDisabled, Speed10Full, Speed100Full}, MaxVLANs: 64}
	plan := MakePlan(desired, FactoryDefault(8), 8, gs308)
	wantOps(t, plan)
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "not supported") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestForcedGigabitMeansAuto(t *testing.T) {
	actual := FactoryDefault(8)
	actual.Ports[4] = PortSettings{Speed: Speed100Full}
	desired := FactoryDefault(8)
	desired.Ports[4] = PortSettings{Speed: Speed1000Full}
	plan := MakePlan(desired, actual, 8, caps)
	wantOps(t, plan, SetPort{Port: 4, Speed: SpeedAuto})
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	wantOps(t, MakePlan(desired, FactoryDefault(8), 8, caps))
}

func TestVLANLimit(t *testing.T) {
	desired := FactoryDefault(8)
	desired.VLANs[3901] = Members{4: T}
	plan := MakePlan(desired, FactoryDefault(8), 8, Capabilities{Speeds: caps.Speeds, MaxVLANs: 1})
	wantOps(t, plan)
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "limit") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestInSyncYieldsEmptyPlan(t *testing.T) {
	wantOps(t, MakePlan(FactoryDefault(8), FactoryDefault(8), 8, caps))
}

func TestDefaultUniFiNameClearsPortName(t *testing.T) {
	actual := FactoryDefault(8)
	actual.Ports[4] = PortSettings{Speed: SpeedAuto, Name: SetName("old-label")}
	desired := ParseUniFi("switch.vlan.1.id=1\nswitch.vlan.1.mode=untagged\nswitch.port.4.name=Port 4", 8).Config
	wantOps(t, MakePlan(desired, actual, 8, caps), SetPort{Port: 4, Speed: SpeedAuto, Name: ""})
	actual.Ports[4] = PortSettings{Speed: SpeedAuto}
	wantOps(t, MakePlan(desired, actual, 8, caps))
}

func TestSwitchNameFromUniFiHostname(t *testing.T) {
	desired := ParseUniFi("switch.vlan.1.id=1\nswitch.vlan.1.mode=untagged\nresolv.host.1.name=SwitchKellerTest", 8).Config
	if desired.SwitchName != SetName("SwitchKellerTest") {
		t.Fatalf("switch name = %v", desired.SwitchName)
	}
	wantOps(t, MakePlan(desired, FactoryDefault(8), 8, caps), SetSwitchName{"SwitchKellerTest"})
	actual := FactoryDefault(8)
	actual.SwitchName = SetName("SwitchKellerTest")
	wantOps(t, MakePlan(desired, actual, 8, caps))
}

func TestSwitchNameIsClippedToWhatTheSwitchAccepts(t *testing.T) {
	desired := FactoryDefault(8)
	desired.SwitchName = SetName("AVeryLongUniFiDeviceNameIndeedü")
	wantOps(t, MakePlan(desired, FactoryDefault(8), 8, caps), SetSwitchName{"AVeryLongUniFiDevice"})
}

func TestNoHostnameLeavesSwitchNameAlone(t *testing.T) {
	actual := FactoryDefault(8)
	actual.SwitchName = SetName("manual")
	wantOps(t, MakePlan(FactoryDefault(8), actual, 8, caps))
}

func TestSwitchSectionKeepsSwitchKeysAndHostnameWithoutSecrets(t *testing.T) {
	cfg := "users.1.password=x\nswitch.wevent.key=k5\nresolv.host.1.name=Sw\n" +
		"resolv.nameserver.1.status=disabled\nnetconf.1.ip=192.0.2.9\nswitch.mtu=9216"
	if got, want := SwitchSection(cfg), "resolv.host.1.name=Sw\nswitch.mtu=9216"; got != want {
		t.Errorf("SwitchSection = %q, want %q", got, want)
	}
}

func TestAdvertisedSpeeds(t *testing.T) {
	gs308 := Capabilities{Speeds: []Speed{SpeedAuto, SpeedDisabled, Speed10Full, Speed100Full}}
	if got := gs308.AdvertisedSpeeds(); !reflect.DeepEqual(got, []Speed{SpeedAuto, Speed10Full, Speed100Full, Speed1000Full}) {
		t.Errorf("advertised = %v", got)
	}
}
