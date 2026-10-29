package device

import (
	"regexp"
	"strings"
)

const redacted = "<redacted>"

// secretKey matches JSON keys and mgmt_cfg lines that carry credentials.
var secretKey = regexp.MustCompile(`(?i)(authkey|password|passphrase|secret|psk|token|^x_|(^|[._])key$)`)

// redactMgmtCfg blanks credential lines of a newline-separated mgmt_cfg body.
func redactMgmtCfg(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if k, _, ok := strings.Cut(line, "="); ok && secretKey.MatchString(k) {
			lines[i] = k + "=" + redacted
		}
	}
	return strings.Join(lines, "\n")
}
