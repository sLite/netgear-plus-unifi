package netgear

import (
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // the switches' login protocol mandates MD5
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Error kinds the poller reacts to. Every error returned by this package
// that matches one of them wraps it (errors.Is).
var (
	// ErrAuthentication: credentials were rejected. Retried slowly to avoid
	// switch lockouts.
	ErrAuthentication = errors.New("switch rejected the login")
	// ErrSessionLimit: the switch refused a new session (e.g. a browser is
	// logged in on a model with a session limit).
	ErrSessionLimit = errors.New("switch reports its maximum number of sessions")
	// ErrSessionDisplaced: the switch dropped our session, typically because
	// someone logged into its web UI on a single-session model (GS116Ev2).
	// Retrying at once would log that person out again.
	ErrSessionDisplaced = errors.New("switch dropped the session (probably a web UI login)")
	// ErrIdentityMismatch: the device at the configured address is not the
	// expected or a supported model.
	ErrIdentityMismatch = errors.New("unexpected switch model")
)

const requestTimeout = 15 * time.Second

// loginRedirect is what a GS116Ev2 returns (HTTP 200) to a displaced
// session: a bare script redirect to the login page.
var loginRedirect = regexp.MustCompile(`(?i)location\.href\s*=\s*['"]/?(?:login\.htm|login\.cgi|wmi/login)['"]`)

// redirectToLoginTitle is what the GS308Ev4 returns for an expired session;
// a transparent re-login is safe there (it allows concurrent sessions).
var redirectToLoginTitle = regexp.MustCompile(`(?i)<title>\s*redirect to login\s*</title>`)

func isLoginRedirect(page []byte) bool {
	return len(page) < 2048 && loginRedirect.Match(page)
}

// field is one form or query parameter. Order matters to the JGS firmware
// (submitId first, submitEnd last), so forms are ordered lists.
type field struct{ key, value string }

func encode(fields []field) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = url.QueryEscape(f.key) + "=" + url.QueryEscape(f.value)
	}
	return strings.Join(parts, "&")
}

// session is one HTTP login session with a switch. It never follows
// redirects and holds exactly one session cookie.
type session struct {
	base   string
	client *http.Client
	// cookie is the raw "name=value" session cookie. The GS308Ev4's SID
	// contains bytes (e.g. a backslash) that net/http's cookie handling drops, so it
	// is parsed and sent verbatim.
	cookie string
	// relogin restores an expired session (GS308Ev4 only); nil disables.
	relogin func(context.Context) error
}

func newSession(host string, transport http.RoundTripper) *session {
	return &session{
		base: "http://" + host + "/",
		client: &http.Client{
			Transport:     transport,
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func (s *session) do(ctx context.Context, method, path string, fields []field) (response, error) {
	target := s.base + path
	var body io.Reader
	if method == http.MethodGet && len(fields) > 0 {
		target += "?" + encode(fields)
	} else if method == http.MethodPost {
		body = strings.NewReader(encode(fields))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return response{}, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if s.cookie != "" {
		req.Header.Set("Cookie", s.cookie)
	}
	started := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Debug("switch request failed", "host", req.URL.Host, "method", method, "path", path, "took", time.Since(started), "err", err)
		return response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	slog.Debug("switch request", "host", req.URL.Host, "method", method, "path", path, "status", resp.StatusCode, "took", time.Since(started))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// JGS firmware answers errors (e.g. 404 for login.cgi) at once but
		// keeps the body open until a timeout; the body is never needed.
		return response{status: resp.StatusCode, header: resp.Header}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return response{status: resp.StatusCode, body: raw, header: resp.Header}, nil
}

// get fetches an authenticated page. An expired session is restored once
// where the model allows it; a displaced session is reported as such.
func (s *session) get(ctx context.Context, path string, query ...field) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		resp, err := s.do(ctx, http.MethodGet, path, query)
		if err != nil {
			return nil, err
		}
		expired := resp.status != http.StatusOK || redirectToLoginTitle.Match(resp.body)
		if expired && attempt == 0 && s.relogin != nil {
			if err := s.relogin(ctx); err != nil {
				return nil, err
			}
			continue
		}
		return checkPage(http.MethodGet, path, resp)
	}
}

// post sends a form. Writes are never repeated automatically.
func (s *session) post(ctx context.Context, path string, form ...field) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodPost, path, form)
	if err != nil {
		return nil, err
	}
	return checkPage(http.MethodPost, path, resp)
}

func checkPage(method, path string, resp response) ([]byte, error) {
	switch {
	case resp.status != http.StatusOK:
		return nil, fmt.Errorf("%s %s: HTTP %d", method, path, resp.status)
	case isLoginRedirect(resp.body), redirectToLoginTitle.Match(resp.body):
		return nil, fmt.Errorf("%s %s: %w", method, path, ErrSessionDisplaced)
	}
	return resp.body, nil
}

// takeCookie stores the session cookie of a login response; false when the
// switch set none (login rejected).
func (s *session) takeCookie(resp response, name string) bool {
	for _, line := range resp.header.Values("Set-Cookie") {
		pair, _, _ := strings.Cut(line, ";")
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && k == name && v != "" {
			s.cookie = k + "=" + v
			return true
		}
	}
	return false
}

// loginFailure classifies a login response that set no session cookie.
func loginFailure(body []byte) error {
	text := strings.ToLower(string(body))
	if strings.Contains(text, "maximum") && strings.Contains(text, "session") {
		return ErrSessionLimit
	}
	return ErrAuthentication
}

// mergeHash is the GS30x login hash: MD5 over the password and the login
// page's rand interleaved character by character.
func mergeHash(password, rand string) string {
	a, b := []rune(password), []rune(rand)
	merged := make([]rune, 0, len(a)+len(b))
	for i := 0; i < len(a) || i < len(b); i++ {
		if i < len(a) {
			merged = append(merged, a[i])
		}
		if i < len(b) {
			merged = append(merged, b[i])
		}
	}
	sum := md5.Sum([]byte(string(merged))) //nolint:gosec // protocol-mandated
	return hex.EncodeToString(sum[:])
}

// hexHMACMD5 is the JGS login hash: HMAC-MD5 with a fixed firmware key over
// the password repeated (NUL-separated) to fill 2048 characters.
func hexHMACMD5(password string) string {
	n := len([]rune(password)) + 1
	repeat := 2048 / n
	msg := strings.Repeat(password+"\x00", repeat) + strings.Repeat("\x00", 2048-repeat*n)
	mac := hmac.New(md5.New, []byte("YOU_CAN_NOT_PASS"))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
