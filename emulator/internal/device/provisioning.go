package device

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"netgear-plus-unifi/emulator/internal/telemetry"
)

// ConfigSink receives UniFi's switch configuration for the physical switch.
// The collector applies it to the switch when writes are enabled.
type ConfigSink interface {
	SubmitConfig(ctx context.Context, switchID, cfgversion, switchConfig string) error
}

// maxAckHold bounds how long a cfgversion is withheld from UniFi while the
// collector applies it, so a stuck collector cannot leave the device in
// "provisioning" forever.
const maxAckHold = 2 * time.Minute

// hostnameKey carries the UniFi device name (hostname-safe: no spaces).
const hostnameKey = "resolv.host.1.name"

// switchSection keeps the switch.* lines and the device hostname of a
// system_cfg document and drops credentials (e.g. switch.wevent.key). Users,
// sshd, ntp and the rest of the document never leave the emulator.
func switchSection(systemCfg string) string {
	var out []string
	for _, line := range strings.Split(systemCfg, "\n") {
		line = strings.TrimSpace(line)
		key, _, ok := strings.Cut(line, "=")
		if ok && (strings.HasPrefix(key, "switch.") || key == hostnameKey) && !secretKey.MatchString(key) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// recordSwitchConfig stores the switch section of a setparam reply. Callers
// hold d.mu; returns whether the stored configuration changed.
func (d *Device) recordSwitchConfig(payload []byte) bool {
	var reply struct {
		CfgVersion string `json:"cfgversion"`
		SystemCfg  string `json:"system_cfg"`
	}
	if json.Unmarshal(payload, &reply) != nil || reply.SystemCfg == "" || reply.CfgVersion == "" {
		return false
	}
	section := switchSection(reply.SystemCfg)
	if section == d.st.SwitchConfig && reply.CfgVersion == d.st.SwitchConfigVersion {
		return false
	}
	d.st.SwitchConfig, d.st.SwitchConfigVersion = section, reply.CfgVersion
	return true
}

// deliverConfig hands the latest switch configuration to the collector until
// the collector reports it as its desired configuration. Re-delivery covers
// collector restarts that lost the configuration.
func (d *Device) deliverConfig(ctx context.Context, sync *telemetry.ConfigSync) {
	if d.opts.Config == nil {
		return
	}
	d.mu.Lock()
	version, text := d.st.SwitchConfigVersion, d.st.SwitchConfig
	delivered := d.delivered
	d.mu.Unlock()
	if version == "" {
		return
	}
	collectorHas := sync != nil && sync.DesiredCfgVersion != nil && *sync.DesiredCfgVersion == version
	if delivered == version && (sync == nil || collectorHas) {
		return
	}
	if err := d.opts.Config.SubmitConfig(ctx, d.sw.ID, version, text); err != nil {
		if ctx.Err() == nil {
			d.log().Warn("could not hand UniFi configuration to the collector", "err", err)
		}
		return
	}
	d.mu.Lock()
	d.delivered = version
	d.mu.Unlock()
	if delivered != version {
		d.log().Info("UniFi switch configuration handed to collector", "cfgversion", version)
	}
}

// heldCfgVersion returns the cfgversion to report instead of the session's,
// or "" to report the session's own. While the collector has not processed
// the latest configuration, UniFi keeps seeing the previously reported one,
// as it would with a real switch that is still provisioning. Callers hold d.mu.
func (d *Device) heldCfgVersion(now time.Time, sync *telemetry.ConfigSync, sessionVersion string) string {
	version := d.st.SwitchConfigVersion
	pending := sync != nil && sync.Enabled && version != "" && version == sessionVersion &&
		(sync.ProcessedCfgVersion == nil || *sync.ProcessedCfgVersion != version)
	if pending && d.st.AckedCfgVersion != "" {
		if d.holdSince.IsZero() {
			d.holdSince = now
		}
		if now.Sub(d.holdSince) <= maxAckHold {
			return d.st.AckedCfgVersion
		}
		if !d.holdExpired {
			d.holdExpired = true
			d.log().Warn("collector has not processed the configuration in time; acknowledging anyway",
				"cfgversion", version)
		}
	} else {
		d.holdSince, d.holdExpired = time.Time{}, false
	}
	if d.st.AckedCfgVersion != sessionVersion {
		if pending || version == sessionVersion {
			d.log().Info("acknowledging UniFi configuration", "cfgversion", sessionVersion)
		}
		d.st.AckedCfgVersion = sessionVersion
		d.saveLocked()
	}
	return ""
}
