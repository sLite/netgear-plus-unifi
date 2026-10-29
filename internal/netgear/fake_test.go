package netgear

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPassword = "test-password"
	// testCookie contains a backslash like the GS308Ev4's real SID, which
	// net/http's cookie handling would drop.
	testCookie = `AbC\de^_f]gh` + "`ij"
)

// fakeSwitch serves the pages recorded from a real switch (testdata/) and
// emulates login, the session cookie and session loss. It records every
// request so tests can check exactly what was sent.
type fakeSwitch struct {
	t     *testing.T
	model string // "gs308ev4" or "gs116ev2"

	mu          sync.Mutex
	loggedIn    bool
	logins      int
	logouts     int
	rejectLogin bool
	maxSessions bool
	// expired makes the next authenticated GET answer like an expired
	// GS308Ev4 session; displaced like a displaced GS116Ev2 session.
	expired, displaced bool
	overrides          map[string][]byte
	// afterPost replaces a page once a form was posted to it.
	afterPost map[string][]byte
	requests  []string
	posts     []post
}

type post struct {
	path string
	form []field
}

func newFakeSwitch(t *testing.T, model string) (*fakeSwitch, *Switch) {
	t.Helper()
	f := &fakeSwitch{t: t, model: model, overrides: map[string][]byte{}, afterPost: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return f, NewSwitch("sw", host, testPassword, "", WithTransport(srv.Client().Transport))
}

func (f *fakeSwitch) page(name string) []byte {
	raw, err := os.ReadFile("testdata/" + f.model + "/" + name + ".html")
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

// pages maps request paths to recorded pages.
func (f *fakeSwitch) pages() map[string]string {
	if f.model == "gs308ev4" {
		return map[string]string{"login.cgi": "login", "dashboard.cgi": "info", "portStatistics.cgi": "stats", "vlan.cgi": "vlan_default"}
	}
	return map[string]string{
		"login.htm": "login", jgsSwitchInfo: "info", jgsPortStatus: "port_status", jgsStatistics: "stats",
		jgsVLANMember: "vlan_membership", jgsVLANConf: "vlan_membership", jgsPVID: "vlan_membership", "index.htm": "port_status",
	}
}

func (f *fakeSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	f.requests = append(f.requests, r.Method+" "+path)
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		f.posts = append(f.posts, post{path, parseOrderedForm(string(body))})
	}
	switch {
	case path == "login.cgi" && f.model == "gs308ev4" && r.Method == http.MethodPost:
		f.login(w, f.lastForm("password") == mergeHash(testPassword, "123456789"))
		return
	case path == "login.htm" && f.model == "gs116ev2" && r.Method == http.MethodPost:
		form := f.posts[len(f.posts)-1].form
		ok := len(form) == 3 && form[0] == field{"submitId", "pwdLogin"} && form[2].key == "submitEnd" &&
			form[1] == field{"password", hexHMACMD5(testPassword)}
		f.login(w, ok)
		return
	case path == "login.cgi" || path == "login.htm":
		if name, ok := f.pages()[path]; ok {
			_, _ = w.Write(f.page(name))
		} else {
			http.NotFound(w, r)
		}
		return
	}
	if !f.loggedIn || r.Header.Get("Cookie") != "SID="+testCookie || f.expired || f.displaced {
		f.sessionLost(w)
		return
	}
	if r.Method == http.MethodPost {
		f.handlePost(w, path)
		return
	}
	if body, ok := f.overrides[path]; ok {
		_, _ = w.Write(body)
		return
	}
	if path == "portStatistics.cgi" && r.URL.Query().Get("hash") != strings.Repeat("0", 32) {
		_, _ = w.Write([]byte("CHECK HASH FAILED"))
		return
	}
	name, ok := f.pages()[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(f.page(name))
}

func (f *fakeSwitch) login(w http.ResponseWriter, ok bool) {
	f.logins++
	switch {
	case f.maxSessions:
		_, _ = w.Write([]byte(`<html><input id="err_msg" value="Maximum number of sessions reached"></html>`))
	case f.rejectLogin || !ok:
		_, _ = w.Write([]byte(`<html><input id="err_msg" value="The password is invalid."></html>`))
	default:
		f.loggedIn, f.expired, f.displaced = true, false, false
		w.Header().Add("Set-Cookie", "SID="+testCookie+";SameSite=Lax;PATH=/;HttpOnly")
		_, _ = w.Write([]byte(`<html><script>document.location.href="/index.cgi";</script></html>`))
	}
}

func (f *fakeSwitch) sessionLost(w http.ResponseWriter) {
	if f.model == "gs308ev4" {
		_, _ = w.Write([]byte(`<html><head><title>Redirect to Login</title></head></html>`))
		return
	}
	_, _ = w.Write([]byte("<html><body><script language=JavaScript> parent.location.href='/login.htm'; </script></body></html>"))
}

func (f *fakeSwitch) handlePost(w http.ResponseWriter, path string) {
	if page, ok := f.afterPost[path]; ok {
		f.overrides[path] = page
	}
	switch path {
	case "logout.cgi":
		f.logouts++
		f.loggedIn = false
	case "index.htm":
		if f.lastForm("submitId") == "logoutBtn" {
			f.logouts++
			f.loggedIn = false
		}
	case "port_status.cgi":
		_, _ = w.Write([]byte("SUCCESS"))
	case "switch_name.cgi":
		_, _ = w.Write([]byte("SWITCH NAME SUCCESS"))
	case "8021qAdvanced.cgi", "portPVID.cgi":
		_, _ = w.Write([]byte("<div>ok</div>"))
	default:
		_, _ = w.Write([]byte("<html></html>"))
	}
}

func (f *fakeSwitch) lastForm(key string) string {
	for _, fl := range f.posts[len(f.posts)-1].form {
		if fl.key == key {
			return fl.value
		}
	}
	return ""
}

func (f *fakeSwitch) set(fn func(*fakeSwitch)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSwitch) snapshot() (requests []string, posts []post, logins, logouts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...), append([]post{}, f.posts...), f.logins, f.logouts
}

// writePosts returns the POSTs other than login and logout.
func (f *fakeSwitch) writePosts() []post {
	_, posts, _, _ := f.snapshot()
	var out []post
	for _, p := range posts {
		if p.path != "login.cgi" && p.path != "login.htm" && p.path != "logout.cgi" &&
			!(p.path == "index.htm" && len(p.form) > 0 && p.form[0].value == "logoutBtn") {
			out = append(out, p)
		}
	}
	return out
}

func parseOrderedForm(body string) []field {
	var out []field
	for pair := range strings.SplitSeq(body, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		out = append(out, field{k, v})
	}
	return out
}

func formMap(form []field) map[string]string {
	out := map[string]string{}
	for _, f := range form {
		out[f.key] = f.value
	}
	return out
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
