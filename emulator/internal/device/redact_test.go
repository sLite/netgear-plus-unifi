package device

import (
	"strings"
	"testing"
)

func TestRedactMgmtCfg(t *testing.T) {
	out := redactMgmtCfg("cfgversion=1\nauthkey=k2\nuse_aes_gcm=true\nswitch.wevent.key=k5")
	for _, secret := range []string{"k2", "k5"} {
		if strings.Contains(out, "="+secret) {
			t.Errorf("secret %s leaked: %s", secret, out)
		}
	}
	for _, kept := range []string{"cfgversion=1", "use_aes_gcm=true"} {
		if !strings.Contains(out, kept) {
			t.Errorf("non-secret %q was removed: %s", kept, out)
		}
	}
}
