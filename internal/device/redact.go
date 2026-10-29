package device

import (
	"strings"

	"netgear-plus-unifi/internal/provision"
)

const redacted = "<redacted>"

// redactMgmtCfg blanks credential lines of a newline-separated mgmt_cfg or
// system_cfg body.
func redactMgmtCfg(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if k, _, ok := strings.Cut(line, "="); ok && provision.IsSecretKey(k) {
			lines[i] = k + "=" + redacted
		}
	}
	return strings.Join(lines, "\n")
}
