// Package device runs one emulated UniFi switch whose port data comes from a
// physical NETGEAR switch. The inform wire protocol and adoption state
// machine come from github.com/jamesbraid/unifi-emu/inform; this package adds
// live telemetry, persistence, the hand-over of UniFi's port configuration
// to the switch and the safety policy (nothing else UniFi sends is applied).
package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/inform"

	"netgear-plus-unifi/internal/config"
	"netgear-plus-unifi/internal/identity"
	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/state"
	"netgear-plus-unifi/internal/telemetry"
)

// Phase is the device's position in the UniFi adoption lifecycle.
type Phase string

const (
	PhasePending   Phase = "pending"
	PhaseAdopting  Phase = "adopting"
	PhaseConnected Phase = "connected"
)

// Interval bounds for controller-supplied inform intervals.
const (
	minInterval = 2 * time.Second
	maxInterval = 5 * time.Minute
)

// Source is the physical switch behind a device: its telemetry and the
// sync that applies UniFi's port configuration to it.
type Source interface {
	// Snapshot returns fresh telemetry, or the reason none is usable.
	Snapshot(now time.Time) (*telemetry.Snapshot, string)
	// SubmitConfig hands over UniFi's switch configuration (switch section
	// of system_cfg) for cfgversion.
	SubmitConfig(cfgversion, switchConfig string)
	// ConfigStatus reports whether a cfgversion has been processed.
	ConfigStatus() provision.Status
}

// Options are the dependencies shared by every device.
type Options struct {
	// InformURL is the resolved (IP-literal) inform endpoint.
	InformURL string
	// ControllerURL is the inform URL as configured; adoptions are bound to
	// it so a controller whose name resolves to a new IP keeps its devices.
	// Defaults to InformURL.
	ControllerURL string
	Interval      time.Duration
	StaleAfter    time.Duration
	Store         *state.Store
	HTTPClient    *http.Client
	Now           func() time.Time
	// PeerMAC returns the virtual MAC of another configured switch, or ""
	// while it is not known yet (physical mode before first telemetry).
	// Used to resolve declared downlinks.
	PeerMAC func(switchID string) string
}

// Device is one emulated switch.
type Device struct {
	sw       config.Switch
	profile  emu.ModelProfile
	physical bool // identity is the NETGEAR's own MAC, learned from telemetry
	src      Source
	opts     Options

	logPtr atomic.Pointer[slog.Logger]

	mu          sync.Mutex
	mac         net.HardwareAddr // nil until known (physical mode)
	macHeader   [6]byte
	sess        *inform.Session
	st          state.Device
	phase       Phase
	interval    time.Duration
	pauseReason string
	lastStatus  int

	delivered   string    // cfgversion last handed to the source
	holdSince   time.Time // when the current cfgversion started being withheld
	holdExpired bool
}

// Status is a read-only view for the status endpoint.
type Status struct {
	SwitchID    string `json:"switch_id"`
	MAC         string `json:"mac"`
	Model       string `json:"model"`
	Phase       Phase  `json:"phase"`
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason,omitempty"`
	LastHTTP    int    `json:"last_http_status,omitempty"`
}

// New builds a device for the switch behind src, resuming persisted
// adoption state when it still matches the configuration. In physical-MAC
// mode the identity is set up from saved state, or from the first telemetry
// if there is none.
func New(sw config.Switch, src Source, opts Options) (*Device, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ControllerURL == "" {
		opts.ControllerURL = opts.InformURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.PeerMAC == nil {
		opts.PeerMAC = func(string) string { return "" }
	}
	profile, err := resolveProfile(sw.UniFi.Model)
	if err != nil {
		return nil, fmt.Errorf("switch %q: %w", sw.ID, err)
	}
	d := &Device{
		sw:       sw,
		profile:  profile,
		physical: identity.IsPhysical(sw.UniFi.MAC),
		src:      src,
		opts:     opts,
		interval: opts.Interval,
		phase:    PhasePending,
	}
	d.logPtr.Store(slog.With("switch", sw.ID))
	if !d.physical {
		mac, err := identity.ResolveMAC(sw.ID, sw.UniFi.MAC)
		if err != nil {
			return nil, fmt.Errorf("switch %q: %w", sw.ID, err)
		}
		return d, d.initIdentity(mac)
	}
	saved, ok, err := opts.Store.Load(sw.ID)
	if err != nil {
		return nil, err
	}
	if ok && saved.MACSource == identity.PhysicalMode {
		if mac, err := identity.ParseHardwareMAC(saved.MAC); err == nil {
			return d, d.initIdentity(mac)
		}
	}
	d.log().Info("waiting for the switch's own MAC before informing (unifi.mac: physical)")
	return d, nil
}

// initIdentity sets the device MAC and resumes or starts its inform session.
func (d *Device) initIdentity(mac net.HardwareAddr) error {
	log := slog.With("switch", d.sw.ID, "mac", mac.String())
	st, err := d.loadState(mac, d.opts.Now(), log)
	if err != nil {
		return err
	}
	desc := inform.Descriptor{
		MAC:          mac.String(),
		Serial:       identity.Serial(mac),
		Model:        d.profile.Model,
		ModelDisplay: d.profile.ModelDisplay,
		Version:      d.profile.Version,
		Hostname:     hostname(d.sw),
		Type:         d.profile.Type,
		FWCaps:       inform.PlaceholderFWCaps,
	}
	if st.FirmwareVersion != "" {
		desc.Version = st.FirmwareVersion
	}
	sess := restoreSession(desc, d.opts.InformURL, st)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mac, d.st, d.sess = mac, st, sess
	d.logPtr.Store(log)
	copy(d.macHeader[:], mac)
	if sess.Adopted() {
		d.phase = PhaseAdopting
		log.Info("resuming adopted device from saved state")
	}
	if d.physical {
		d.saveLocked() // remember the learned MAC for restarts
	}
	return nil
}

// loadState returns persisted state, discarding an adoption that belongs to
// a different MAC, MAC source, model or controller than the configuration.
// The binding to the physical switch is kept in every case.
func (d *Device) loadState(mac net.HardwareAddr, now time.Time, log *slog.Logger) (state.Device, error) {
	fresh := state.Device{SwitchID: d.sw.ID, MAC: mac.String(), Model: d.sw.UniFi.Model, BootTime: now}
	if d.physical {
		fresh.MACSource = identity.PhysicalMode
	}
	st, ok, err := d.opts.Store.Load(d.sw.ID)
	if err != nil || !ok {
		return fresh, err
	}
	fresh.PhysicalModel, fresh.PhysicalSerial = st.PhysicalModel, st.PhysicalSerial
	switch {
	case st.MAC != fresh.MAC || st.MACSource != fresh.MACSource || st.Model != fresh.Model:
		log.Warn("saved state is for a different virtual MAC or model; starting as a new device",
			"saved_mac", st.MAC, "saved_model", st.Model)
		return fresh, nil
	case st.Adopted && st.BootstrapInformURL != "" && st.BootstrapInformURL != d.opts.ControllerURL:
		log.Warn("inform URL changed since adoption; discarding adoption, the device must be adopted again",
			"adopted_via", st.BootstrapInformURL, "configured", d.opts.ControllerURL)
		fresh.BootTime = st.BootTime
		return fresh, nil
	}
	if st.BootTime.IsZero() {
		st.BootTime = now
	}
	return st, nil
}

func hostname(sw config.Switch) string {
	if sw.UniFi.Name != "" {
		return sw.UniFi.Name
	}
	return sw.ID
}

// log returns the device logger (tagged with the MAC once it is known).
func (d *Device) log() *slog.Logger { return d.logPtr.Load() }

// MAC returns the virtual MAC, or "" while it is not known yet.
func (d *Device) MAC() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mac == nil {
		return ""
	}
	return d.mac.String()
}

// Status returns a snapshot of the device runtime state.
func (d *Device) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{
		SwitchID: d.sw.ID, MAC: macString(d.mac), Model: d.sw.UniFi.Model,
		Phase: d.phase, Paused: d.pauseReason != "", PauseReason: d.pauseReason, LastHTTP: d.lastStatus,
	}
}

// Run informs until ctx is cancelled.
func (d *Device) Run(ctx context.Context) {
	d.log().Info("device started", "model", d.sw.UniFi.Model, "phase", d.Status().Phase)
	for {
		d.Step(ctx)
		d.mu.Lock()
		wait := d.interval
		d.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Step performs one inform cycle: check telemetry, inform, apply the reply.
func (d *Device) Step(ctx context.Context) {
	now := d.opts.Now()
	if d.MAC() == "" {
		if reason := d.learnMAC(now); reason != "" {
			d.setPaused(reason)
			return
		}
	}
	snap, reason := d.usableSnapshot(now)
	d.setPaused(reason)
	if snap == nil {
		return
	}
	d.deliverConfig()
	pkt, key, sentAdopted, err := d.encodeInform(now, snap, d.resolveDownlinks())
	if err != nil {
		d.log().Error("build inform", "err", err)
		return
	}
	status, body, err := d.post(ctx, pkt)
	if err != nil {
		if ctx.Err() == nil {
			d.log().Warn("inform failed", "err", err)
		}
		return
	}
	d.noteHTTPStatus(status, body)
	if status != http.StatusOK || len(body) == 0 {
		return
	}
	dec, err := inform.Decode(body, key)
	if err != nil {
		d.log().Warn("undecodable controller reply", "err", err)
		return
	}
	d.handleReply(d.opts.Now(), dec.Payload, sentAdopted)
	d.deliverConfig() // hand new configuration to the switch without waiting a cycle
}

// learnMAC sets up the identity from the switch's own MAC (physical mode).
// It returns why that is not possible yet, or "" on success.
func (d *Device) learnMAC(now time.Time) string {
	snap, reason := d.src.Snapshot(now)
	if snap == nil {
		return "waiting for the switch's own MAC: " + reason
	}
	mac, err := identity.ParseHardwareMAC(snap.Device.HardwareMAC)
	if err != nil {
		return err.Error()
	}
	if err := d.initIdentity(mac); err != nil {
		return "set up device identity: " + err.Error()
	}
	d.log().Info("using the switch's own MAC as device identity (unifi.mac: physical)")
	return ""
}

// resolveDownlinks looks up the MACs of declared downstream switches. A peer
// whose MAC is not known yet is skipped until it is.
func (d *Device) resolveDownlinks() []neighbor {
	var out []neighbor
	for _, dl := range d.sw.UniFi.Downlinks {
		if mac := d.opts.PeerMAC(dl.Switch); mac != "" {
			out = append(out, neighbor{Port: dl.Port, MAC: mac})
		}
	}
	return out
}

// usableSnapshot returns fresh telemetry for the bound physical switch, or
// the reason none is usable. Without fresh telemetry the device does not
// inform at all, so UniFi marks it offline instead of showing stale ports.
func (d *Device) usableSnapshot(now time.Time) (*telemetry.Snapshot, string) {
	snap, reason := d.src.Snapshot(now)
	if snap == nil {
		return nil, reason
	}
	if len(snap.Ports) == 0 {
		return nil, "switch reported no ports"
	}
	if reason := d.bindPhysical(snap.Device); reason != "" {
		return nil, reason
	}
	if d.physical {
		if mac, err := identity.ParseHardwareMAC(snap.Device.HardwareMAC); err == nil && mac.String() != d.MAC() {
			return nil, fmt.Sprintf("switch now reports MAC %s but the device was adopted as %s; run `netgear-unifi reset %s` if intended",
				mac, d.MAC(), d.sw.ID)
		}
	}
	return snap, ""
}

// bindPhysical ties the virtual device to the first physical switch seen and
// refuses telemetry from any other, e.g. after an IP address was reassigned.
func (d *Device) bindPhysical(dev telemetry.Device) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st.PhysicalModel == "" {
		d.st.PhysicalModel, d.st.PhysicalSerial = dev.Model, dev.Serial
		d.saveLocked()
		d.log().Info("bound to physical switch", "physical_model", dev.Model, "firmware", dev.Firmware, "ip", dev.ManagementIP)
		return ""
	}
	if dev.Model != d.st.PhysicalModel || (d.st.PhysicalSerial != "" && dev.Serial != "" && dev.Serial != d.st.PhysicalSerial) {
		return fmt.Sprintf("physical switch identity changed (bound %s, now %s); run `netgear-unifi reset %s` if intended",
			d.st.PhysicalModel, dev.Model, d.sw.ID)
	}
	return ""
}

func (d *Device) setPaused(reason string) {
	d.mu.Lock()
	prev := d.pauseReason
	d.pauseReason = reason
	startup := d.lastStatus == 0 // nothing sent yet since process start
	d.mu.Unlock()
	switch {
	case reason != "" && prev == "" && startup:
		d.log().Info("waiting for telemetry before the first inform", "reason", reason)
	case reason != "" && prev == "":
		d.log().Warn("pausing informs; UniFi will show the switch offline", "reason", reason)
	case reason == "" && prev != "" && startup:
		d.log().Info("telemetry available; informing")
	case reason == "" && prev != "":
		d.log().Info("telemetry available; informing resumed")
	}
}

func (d *Device) encodeInform(now time.Time, snap *telemetry.Snapshot, downlinks []neighbor) (pkt []byte, key string, adopted bool, err error) {
	status := d.src.ConfigStatus()
	d.mu.Lock()
	defer d.mu.Unlock()
	adopted = d.sess.Adopted()
	base := d.sess.BuildPayload(now)
	var session struct {
		CfgVersion string `json:"cfgversion"`
	}
	_ = json.Unmarshal(base, &session)
	payload, err := overlayPayload(base, overlayInput{
		CfgVersion:  d.heldCfgVersion(now, status, session.CfgVersion),
		Adopted:     adopted,
		MAC:         d.mac.String(),
		IP:          d.reportedIP(snap),
		Snapshot:    snap,
		Downlinks:   downlinks,
		Provisioned: d.st.Provisioned,
	})
	if err != nil {
		return nil, "", false, err
	}
	key = d.sess.AuthKey()
	p := &inform.Packet{MAC: d.macHeader, Payload: payload}
	if d.sess.UseAESGCM() {
		pkt, err = p.EncodeGCM(key)
	} else {
		pkt, err = p.Encode(key)
	}
	return pkt, key, adopted, err
}

func (d *Device) reportedIP(snap *telemetry.Snapshot) string {
	if ip := d.sw.UniFi.IP; ip != "" && ip != "physical" {
		return ip
	}
	return snap.Device.ManagementIP
}

func (d *Device) post(ctx context.Context, pkt []byte) (int, []byte, error) {
	d.mu.Lock()
	url := d.sess.InformURL()
	d.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(pkt))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-binary")
	resp, err := d.opts.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, err
}

// noteHTTPStatus logs status transitions only; a pending device answers 404
// on every inform until someone clicks Adopt.
func (d *Device) noteHTTPStatus(status int, body []byte) {
	d.mu.Lock()
	prev := d.lastStatus
	d.lastStatus = status
	adopted := d.sess.Adopted()
	d.mu.Unlock()
	if status == prev {
		return
	}
	switch {
	case status == http.StatusNotFound && adopted:
		d.log().Warn("controller answers 404 to an adopted device; if it was removed in UniFi, run `netgear-unifi reset` for it")
	case status == http.StatusNotFound:
		d.log().Info("pending adoption: waiting for Adopt in UniFi Network")
	case status != http.StatusOK:
		d.log().Warn("controller rejected inform", "http_status", status, "body", truncate(body, 200))
	default:
		d.log().Debug("inform accepted", "http_status", status)
	}
}

// handleReply records provisioning, advances the session and persists any
// state change. Only the switch section of system_cfg is forwarded to the
// physical switch (via deliverConfig); nothing else in a reply is applied.
func (d *Device) handleReply(now time.Time, payload []byte, sentAdopted bool) {
	var head struct {
		Type string `json:"_type"`
		Cmd  string `json:"cmd"`
	}
	_ = json.Unmarshal(payload, &head)

	d.mu.Lock()
	defer d.mu.Unlock()
	before := d.st
	for _, e := range d.sess.Apply(now, payload) {
		d.logEffect(e)
	}
	if head.Type == "setparam" {
		var sp struct {
			SystemCfg string `json:"system_cfg"`
		}
		if json.Unmarshal(payload, &sp) == nil && sp.SystemCfg != "" {
			d.log().Debug("system_cfg received", "system_cfg", redactMgmtCfg(sp.SystemCfg))
		}
	}
	if head.Type == "setparam" && d.sess.Adopted() && d.recordSwitchConfig(payload) {
		d.log().Info("UniFi sent switch configuration", "cfgversion", d.st.SwitchConfigVersion)
	}
	next := captureState(d.sess, now, d.st)
	if next.Adopted && !before.Adopted {
		next.BootstrapInformURL = d.opts.ControllerURL
	}
	if head.Type == "setstate" && next.Adopted {
		var tables map[string]json.RawMessage
		if json.Unmarshal(payload, &tables) == nil && recordProvisioned(&next, tables) {
			d.log().Info("controller provisioned setstate tables; echoed back, not applied to the switch")
		}
	}
	d.st = next
	if !sameState(before, next) {
		d.saveLocked()
	}
	d.advancePhase(sentAdopted)
}

func (d *Device) advancePhase(sentAdopted bool) {
	prev := d.phase
	switch {
	case !d.sess.Adopted():
		d.phase = PhasePending
	case sentAdopted:
		d.phase = PhaseConnected
	default:
		d.phase = PhaseAdopting
	}
	if d.phase != prev {
		d.log().Info("adoption phase changed", "from", prev, "to", d.phase)
	}
}

func (d *Device) logEffect(e inform.Effect) {
	switch e.Kind {
	case inform.EffectAdoptingViaSetAdopt, inform.EffectAdoptingViaMgmtCfg:
		d.log().Info("controller issued adoption key")
	case inform.EffectFactoryReset:
		d.log().Warn("controller sent setdefault (device forgotten); back to pending, physical switch untouched")
	case inform.EffectRebooted:
		d.log().Info("controller requested reboot; emulated only, physical switch untouched")
	case inform.EffectUpgraded:
		d.log().Warn("controller requested firmware upgrade; emulated only, NETGEAR firmware is never touched", "target", e.Text)
	case inform.EffectInterval:
		d.interval = min(max(e.Interval, minInterval), maxInterval)
	case inform.EffectMgmtCfg:
		d.log().Debug("mgmt_cfg received", "mgmt_cfg", redactMgmtCfg(e.Text))
	case inform.EffectUnknownCmd:
		d.log().Info("ignoring controller command", "cmd", e.Text)
	case inform.EffectUnknownType:
		d.log().Info("ignoring controller reply type", "type", e.Text)
	case inform.EffectDecodeError:
		d.log().Warn("ignoring undecodable controller reply", "err", e.Text)
	}
}

// saveLocked persists d.st; callers hold d.mu.
func (d *Device) saveLocked() {
	if err := d.opts.Store.Save(d.st); err != nil {
		d.log().Error("persist device state", "err", err)
	}
}

func macString(mac net.HardwareAddr) string {
	if mac == nil {
		return ""
	}
	return mac.String()
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// resolveProfile validates the UniFi model against the unifi-emu model
// registry; only switch models are accepted.
func resolveProfile(model string) (emu.ModelProfile, error) {
	p, ok := emu.Profile(model)
	if !ok {
		return emu.ModelProfile{}, fmt.Errorf("unknown UniFi model %q", model)
	}
	if p.Type != "usw" {
		return emu.ModelProfile{}, errors.New("UniFi model " + model + " is not a switch (type " + p.Type + ")")
	}
	return p, nil
}
