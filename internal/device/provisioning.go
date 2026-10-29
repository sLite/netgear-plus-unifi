package device

import (
	"encoding/json"
	"time"

	"netgear-plus-unifi/internal/provision"
)

// maxAckHold bounds how long a cfgversion is withheld from UniFi while the
// switch is being provisioned, so a stuck sync cannot leave the device in
// "provisioning" forever.
const maxAckHold = 2 * time.Minute

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
	section := provision.SwitchSection(reply.SystemCfg)
	if section == d.st.SwitchConfig && reply.CfgVersion == d.st.SwitchConfigVersion {
		return false
	}
	d.st.SwitchConfig, d.st.SwitchConfigVersion = section, reply.CfgVersion
	return true
}

// deliverConfig hands the latest switch configuration to the source. The
// source ignores identical resubmissions; after a restart the first call
// re-submits the stored configuration, which makes the sync re-verify the
// switch.
func (d *Device) deliverConfig() {
	d.mu.Lock()
	version, text, delivered := d.st.SwitchConfigVersion, d.st.SwitchConfig, d.delivered
	d.delivered = version
	d.mu.Unlock()
	if version != "" && version != delivered {
		d.src.SubmitConfig(version, text)
	}
}

// heldCfgVersion returns the cfgversion to report instead of the session's,
// or "" to report the session's own. While the switch has not processed the
// latest configuration, UniFi keeps seeing the previously reported one, as
// it would with a real switch that is still provisioning. Callers hold d.mu.
func (d *Device) heldCfgVersion(now time.Time, sync provision.Status, sessionVersion string) string {
	version := d.st.SwitchConfigVersion
	pending := sync.Enabled && version != "" && version == sessionVersion && !sync.Processed(version)
	if pending && d.st.AckedCfgVersion != "" {
		if d.holdSince.IsZero() {
			d.holdSince = now
		}
		if now.Sub(d.holdSince) <= maxAckHold {
			return d.st.AckedCfgVersion
		}
		if !d.holdExpired {
			d.holdExpired = true
			d.log().Warn("switch has not processed the configuration in time; acknowledging anyway",
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
