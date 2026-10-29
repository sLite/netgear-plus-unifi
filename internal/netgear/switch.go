// Package netgear talks to NETGEAR Plus switches over their web UI: model
// detection, login, status pages and (opt-in) configuration writes. Two
// models are supported, both verified on real hardware:
//
//   - GS308Ev4 (firmware V1.0.1.9): GS30x-style CGI pages.
//   - GS116Ev2 (firmware 2.6.0.50): JGS-style form pages.
//
// The login hashing, page layouts and status parsing follow
// py-netgear-plus (commit 552b041, Apache-2.0), which this package replaces;
// the write protocols were taken from the switches' own web-UI code (see
// docs/CONFIG-WRITE.md).
package netgear

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

// metadataRefresh is how often model metadata (firmware, serial, MAC) is
// re-read where it lives on a separate page.
const metadataRefresh = 10 * time.Minute

// models are the supported switch models. Adding a model means adding its
// implementation here (see AGENTS.md).
var models = []model{gs308ev4{}, gs116ev2{}}

// SupportedModels returns the names of the supported models.
func SupportedModels() []string {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.name()
	}
	return names
}

// model is one supported switch model.
type model interface {
	name() string
	ports() int
	// verifiedFirmware is the firmware the page formats were verified
	// with; other versions work but log a warning.
	verifiedFirmware() string
	caps() provision.Capabilities
	// identify reports whether a login page belongs to this model.
	identify(p loginPage) bool
	// reloginOnExpiry allows restoring an expired session transparently.
	// Must be false for single-session models, where logging in again
	// would log out an administrator using the web UI.
	reloginOnExpiry() bool
	login(ctx context.Context, s *session, password string) error
	logout(ctx context.Context, s *session)
	// read returns the port readings and, when withMetadata (or when the
	// metadata is on the status page anyway), the device metadata.
	read(ctx context.Context, s *session, withMetadata bool) (*metadata, []telemetry.PortReading, error)
	// backend returns the write protocol, or nil for a read-only model.
	backend(s *session) provision.Backend
}

// metadata is what a switch reports about itself.
type metadata struct {
	firmware, serial, mac string
}

// Switch polls one NETGEAR switch, keeping one login session open between
// polls. Not safe for concurrent use: the poller owns it, and configuration
// writes run on the poller goroutine between reads.
type Switch struct {
	id, host, password, expected string
	transport                    http.RoundTripper
	now                          func() time.Time
	log                          *slog.Logger

	sess     *session
	model    model
	device   *telemetry.Device
	deviceAt time.Time
}

// Option customises a Switch (tests).
type Option func(*Switch)

// WithTransport sets the HTTP transport.
func WithTransport(t http.RoundTripper) Option { return func(s *Switch) { s.transport = t } }

// WithClock sets the clock used for metadata refresh and readings.
func WithClock(now func() time.Time) Option { return func(s *Switch) { s.now = now } }

// NewSwitch returns a switch at host. expectedModel, when set, refuses any
// other model before logging in.
func NewSwitch(id, host, password, expectedModel string, opts ...Option) *Switch {
	s := &Switch{
		id: id, host: host, password: password, expected: expectedModel,
		transport: http.DefaultTransport, now: time.Now, log: slog.With("switch", id),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Read returns the current state of every port. On any error the session
// is dropped; the next Read logs in again.
func (s *Switch) Read(ctx context.Context) (telemetry.Reading, error) {
	if s.sess == nil {
		if err := s.connect(ctx); err != nil {
			s.Close()
			return telemetry.Reading{}, err
		}
	}
	r, err := s.readOnce(ctx)
	if err != nil {
		s.Close()
		return telemetry.Reading{}, err
	}
	return r, nil
}

// Writer returns a configuration writer on the current session, or false
// when no session is open or the model has no write support. Writes reuse
// the polling session because the GS116Ev2 allows only one.
func (s *Switch) Writer() (provision.Writer, bool) {
	if s.sess == nil {
		return nil, false
	}
	b := s.model.backend(s.sess)
	if b == nil {
		return nil, false
	}
	return provision.NewVerifiedWriter(b, s.model.ports(), s.model.caps(), s.log.With("host", s.host)), true
}

// Close logs out, freeing the switch's session slot. Never fails.
func (s *Switch) Close() {
	sess := s.sess
	s.sess = nil
	if sess == nil || sess.cookie == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	s.model.logout(ctx, sess)
	s.log.Debug("logged out")
}

func (s *Switch) connect(ctx context.Context) error {
	sess := newSession(s.host, s.transport)
	m, err := detect(ctx, sess)
	if err != nil {
		return err
	}
	if s.expected != "" && m.name() != s.expected {
		return fmt.Errorf("%w: %s is a %s, configuration expects %s", ErrIdentityMismatch, s.host, m.name(), s.expected)
	}
	s.sess, s.model, s.device = sess, m, nil
	if err := m.login(ctx, sess, s.password); err != nil {
		return err
	}
	if m.reloginOnExpiry() {
		sess.relogin = func(ctx context.Context) error { return m.login(ctx, sess, s.password) }
	}
	s.log.Info("logged in", "host", s.host, "model", m.name())
	return nil
}

func (s *Switch) readOnce(ctx context.Context) (telemetry.Reading, error) {
	due := s.device == nil || s.now().Sub(s.deviceAt) > metadataRefresh
	meta, ports, err := s.model.read(ctx, s.sess, due)
	if err != nil {
		return telemetry.Reading{}, err
	}
	if len(ports) != s.model.ports() {
		return telemetry.Reading{}, fmt.Errorf("switch reported %d ports, %s has %d", len(ports), s.model.name(), s.model.ports())
	}
	if meta != nil && (due || s.device == nil) {
		s.device = s.deviceInfo(meta)
		s.deviceAt = s.now()
	}
	if s.device == nil {
		return telemetry.Reading{}, errors.New("switch metadata missing")
	}
	return telemetry.Reading{Device: *s.device, Ports: ports, At: s.now()}, nil
}

func (s *Switch) deviceInfo(meta *metadata) *telemetry.Device {
	name := s.model.name()
	if want := s.model.verifiedFirmware(); meta.firmware != "" && meta.firmware != want {
		s.log.Warn("firmware differs from the verified version; page formats may differ",
			"firmware", meta.firmware, "verified", want)
	}
	var speeds []string
	for _, sp := range s.model.caps().AdvertisedSpeeds() {
		speeds = append(speeds, string(sp))
	}
	return &telemetry.Device{
		Vendor: "netgear", Model: name, Firmware: meta.firmware, Serial: meta.serial,
		ManagementIP: s.host, PortCount: s.model.ports(), HardwareMAC: meta.mac, PortSpeeds: speeds,
	}
}

// loginPage is what model detection looks at: GS30x-style pages carry a
// login "rand" and the model in the title, JGS-style pages a sysGeneInfor
// record starting with the model.
type loginPage struct {
	rand, title, sysModel string
}

func parseLoginPage(body []byte) loginPage {
	doc := parseHTML(body)
	p := loginPage{
		rand:  doc.first(tag("input"), attrIs("id", "rand")).val("value"),
		title: strings.TrimSpace(strings.ReplaceAll(doc.first(tag("title")).text(), "NETGEAR", "")),
	}
	if info := sysGeneInfor.FindSubmatch(body); info != nil {
		p.sysModel, _, _ = strings.Cut(string(info[1]), "?")
	}
	return p
}

// detect identifies the model from its login page, like py-netgear-plus.
func detect(ctx context.Context, s *session) (model, error) {
	var lastErr error
	for _, path := range []string{"login.cgi", "login.htm", ""} {
		resp, err := s.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.status != http.StatusOK {
			continue
		}
		page := parseLoginPage(resp.body)
		for _, m := range models {
			if m.identify(page) {
				return m, nil
			}
		}
		if name := cmp.Or(page.sysModel, page.title); page.rand != "" && page.title != "" || page.sysModel != "" {
			return nil, fmt.Errorf("%w: %s is not supported (supported: %s)", ErrIdentityMismatch, name, strings.Join(SupportedModels(), ", "))
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: no NETGEAR Plus login page found", ErrIdentityMismatch)
}

// Shared parsing of status values.

var sysGeneInfor = regexp.MustCompile(`sysGeneInfor = '([^']+)';`)

// connectedStates are the port status texts meaning "link up".
var connectedStates = map[string]bool{"Aktiv": true, "Up": true, "UP": true, "CONNECTED": true}

var linkSpeeds = map[string]int{
	"10G": 10_000, "5G": 5_000, "2.5G": 2_500, "1G": 1_000, "1000M": 1_000, "100M": 100, "10M": 10,
}

var duplexWords = regexp.MustCompile(`(?i)\b(full|half)\b`)

// parseLinkSpeed maps NETGEAR speed text ("1000M full", "100M", "No Speed")
// to Mbps: 0 for "no link" texts, nil for anything unrecognised.
func parseLinkSpeed(text string) *int {
	token := strings.ToUpper(strings.TrimSpace(duplexWords.ReplaceAllString(text, "")))
	switch token {
	case "NO SPEED", "", "LINK DOWN", "DOWN":
		return ptr(0)
	}
	if v, ok := linkSpeeds[token]; ok {
		return ptr(v)
	}
	return nil
}

// parseDuplex returns true/false for "... full"/"... half", nil otherwise.
func parseDuplex(text string) *bool {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "full"):
		return ptr(true)
	case strings.Contains(lower, "half"):
		return ptr(false)
	}
	return nil
}

var macPattern = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

func hardwareMAC(text string) string {
	text = strings.TrimSpace(text)
	if macPattern.MatchString(text) {
		return strings.ToLower(text)
	}
	return ""
}

func ptr[T any](v T) *T { return &v }

func parseUint(s string) (*uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}
