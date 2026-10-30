// Command unifi-testctl drives a DISPOSABLE UniFi Network test controller
// from the command line (development tool, not part of the image).
//
//	unifi-testctl devices                 list devices and their state
//	unifi-testctl ports MAC               show the port table UniFi stores
//	unifi-testctl adopt MAC [MAC...]      adopt pending devices
//	unifi-testctl wait MAC [MAC...]       wait until devices are connected
//	unifi-testctl forget MAC [MAC...]     remove devices from the controller
//	unifi-testctl name MAC NAME           rename a device
//	unifi-testctl network NAME VLAN       create a VLAN-only network (idempotent)
//	unifi-testctl network-delete VLAN     delete the network with that VLAN id
//	unifi-testctl port MAC PORT [native=VID] [tagged=all|none|VID,VID] [state=disabled]
//	                      [speed=auto|10|100|1000] [duplex=full|half] [name=TEXT]
//	                                      set a port override, like the port editor in the UI
//	unifi-testctl port-reset MAC PORT     remove a port override (back to defaults)
//	unifi-testctl get PATH                print the JSON of a site API path (e.g. stat/device,
//	                                      v2/firewall-policies)
//
// Environment: UNIFI_URL (default https://127.0.0.1:18443), UNIFI_USER
// (admin), UNIFI_PASSWORD (default: the seeded test image's password),
// UNIFI_SITE (default).
//
// Safety: refuses any controller outside loopback/docker networks unless
// ALLOW_NON_TEST_CONTROLLER=1 is set, so it cannot touch a production
// controller by accident. Even then, a non-test controller is read-only:
// apart from the login, only GET requests are sent (devices, ports, get).
// UniFi OS consoles (e.g. a Dream Router) are detected at login. TLS
// verification is off because controllers use self-signed certificates.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var states = map[int]string{0: "disconnected", 1: "connected", 2: "pending", 4: "upgrading", 5: "provisioning",
	6: "heartbeat-missed", 7: "adopt-failed", 10: "adopting"}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	base := strings.TrimRight(envOr("UNIFI_URL", "https://127.0.0.1:18443"), "/")
	test, err := guard(base)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New(usage())
	}
	c, err := login(base, !test)
	if err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]
	switch {
	case cmd == "get" && len(rest) == 1:
		path := strings.TrimPrefix(rest[0], "/")
		var out json.RawMessage
		if v2, ok := strings.CutPrefix(path, "v2/"); ok {
			// v2 endpoints answer bare JSON instead of {"data": ...}.
			raw, err := c.raw("GET", "/v2/api/site/"+c.site+"/"+v2)
			if err != nil {
				return err
			}
			fmt.Println(string(raw))
			return nil
		}
		if err := c.call("GET", "/"+path, nil, &out); err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	case cmd == "devices":
		return c.showDevices()
	case cmd == "ports" && len(rest) == 1:
		return c.showPorts(rest[0])
	case cmd == "adopt":
		for _, mac := range rest {
			if err := c.call("POST", "/cmd/devmgr", map[string]any{"cmd": "adopt", "mac": strings.ToLower(mac)}, nil); err != nil {
				return err
			}
			fmt.Println("adopt requested for", mac)
			time.Sleep(2 * time.Second) // controllers reject adoption bursts
		}
		return nil
	case cmd == "wait":
		return c.waitConnected(rest, 5*time.Minute)
	case cmd == "forget":
		for _, mac := range rest {
			if err := c.call("POST", "/cmd/sitemgr", map[string]any{"cmd": "delete-device", "mac": strings.ToLower(mac)}, nil); err != nil {
				return err
			}
			fmt.Println("forgot", mac)
		}
		return nil
	case cmd == "name" && len(rest) == 2:
		d, err := c.device(rest[0])
		if err != nil {
			return err
		}
		if err := c.call("PUT", "/rest/device/"+d.str("_id"), map[string]any{"name": rest[1]}, nil); err != nil {
			return err
		}
		fmt.Printf("%s renamed to %q\n", rest[0], rest[1])
		return nil
	case cmd == "network" && len(rest) == 2:
		return c.createNetwork(rest[0], rest[1])
	case cmd == "network-delete" && len(rest) == 1:
		return c.deleteNetwork(rest[0])
	case cmd == "port" && len(rest) >= 2:
		return c.setPort(rest[0], rest[1], rest[2:])
	case cmd == "port-reset" && len(rest) == 2:
		return c.setOverride(rest[0], rest[1], nil)
	}
	return errors.New(usage())
}

func usage() string {
	return "usage: unifi-testctl devices | ports MAC | adopt MAC... | wait MAC... | forget MAC... | name MAC NAME | " +
		"network NAME VLAN | network-delete VLAN | port MAC PORT [key=value...] | port-reset MAC PORT | get PATH"
}

// guard refuses controllers outside loopback and private docker networks,
// unless explicitly allowed; test reports whether base is a test controller.
func guard(base string) (test bool, err error) {
	u, err := url.Parse(base)
	if err != nil {
		return false, err
	}
	host := u.Hostname()
	test = host == "localhost" || host == "unifi-controller"
	if ip := net.ParseIP(host); ip != nil {
		_, docker, _ := net.ParseCIDR("172.16.0.0/12")
		test = ip.IsLoopback() || docker.Contains(ip)
	}
	if !test && os.Getenv("ALLOW_NON_TEST_CONTROLLER") != "1" {
		return false, fmt.Errorf("refusing %s: not a local test controller (set ALLOW_NON_TEST_CONTROLLER=1)", base)
	}
	return test, nil
}

type controller struct {
	base, site string
	hc         *http.Client
	// network is the path prefix of the Network application: "" on a
	// standalone controller, "/proxy/network" on a UniFi OS console.
	network string
	// readOnly permits only GET requests after the login.
	readOnly bool
}

// login signs in, trying the UniFi OS login first and the standalone
// controller login second.
func login(base string, readOnly bool) (*controller, error) {
	jar, _ := cookiejar.New(nil)
	c := &controller{base: base, site: envOr("UNIFI_SITE", "default"), readOnly: readOnly, hc: &http.Client{
		Jar: jar, Timeout: 20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // self-signed controllers
	}}
	creds := map[string]any{"username": envOr("UNIFI_USER", "admin"), "password": envOr("UNIFI_PASSWORD", "unifi-containers-seeded")}
	if err := c.send("POST", "/api/auth/login", creds, nil); err == nil {
		c.network = "/proxy/network"
		return c, nil
	}
	if err := c.send("POST", "/api/login", creds, nil); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	return c, nil
}

// call sends a request to a site-scoped API path.
func (c *controller) call(method, path string, body, out any) error {
	return c.request(method, "/api/s/"+c.site+path, body, out)
}

// request sends a request to a Network application path.
func (c *controller) request(method, path string, body, out any) error {
	if c.readOnly && method != "GET" {
		return fmt.Errorf("refusing %s %s: read-only on a non-test controller", method, path)
	}
	return c.send(method, c.network+path, body, out)
}

// raw returns the body of a Network application request as is.
func (c *controller) raw(method, path string) ([]byte, error) {
	if c.readOnly && method != "GET" {
		return nil, fmt.Errorf("refusing %s %s: read-only on a non-test controller", method, path)
	}
	req, err := http.NewRequest(method, c.base+c.network+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return body, nil
}

func (c *controller) send(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, out)
}

type device map[string]any

func (d device) str(k string) string { s, _ := d[k].(string); return s }
func (d device) num(k string) int    { f, _ := d[k].(float64); return int(f) }

func (d device) ports() []map[string]any {
	rows, _ := d["port_table"].([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (c *controller) devices() ([]device, error) {
	var out []device
	return out, c.call("GET", "/stat/device", nil, &out)
}

func (c *controller) device(mac string) (device, error) {
	devs, err := c.devices()
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if d.str("mac") == strings.ToLower(mac) {
			return d, nil
		}
	}
	return nil, fmt.Errorf("%s not found", mac)
}

func (c *controller) showDevices() error {
	devs, err := c.devices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		up := 0
		for _, p := range d.ports() {
			if p["up"] == true {
				up++
			}
		}
		name := d.str("name")
		if name == "" {
			name = d.str("hostname")
		}
		state, ok := states[d.num("state")]
		if !ok {
			state = strconv.Itoa(d.num("state"))
		}
		fmt.Printf("%s  %-8s %-12s %-20s ip=%s ports_up=%d/%d version=%s\n",
			d.str("mac"), d.str("model"), state, name, d.str("ip"), up, len(d.ports()), d.str("version"))
	}
	return nil
}

func (c *controller) showPorts(mac string) error {
	d, err := c.device(mac)
	if err != nil {
		return err
	}
	for _, p := range d.ports() {
		fmt.Printf("port %2v up=%-5v speed=%-5v fdx=%-5v rx=%v tx=%v rx/s=%v tx/s=%v poe_caps=%v name=%q\n",
			p["port_idx"], p["up"], p["speed"], p["full_duplex"], p["rx_bytes"], p["tx_bytes"],
			p["rx_bytes-r"], p["tx_bytes-r"], p["poe_caps"], p["name"])
	}
	return nil
}

func (c *controller) waitConnected(macs []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, mac := range macs {
		for {
			d, err := c.device(mac)
			if err == nil && d.num("state") == 1 {
				fmt.Println(mac, "connected")
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s not connected", mac)
			}
			time.Sleep(3 * time.Second)
		}
	}
	return nil
}

type network map[string]any

func (c *controller) networks() ([]network, error) {
	var out []network
	return out, c.call("GET", "/rest/networkconf", nil, &out)
}

func vlanOf(n network) int {
	if v, ok := n["vlan"].(float64); ok && v > 0 {
		return int(v)
	}
	return 1
}

// networkID finds the network for a VLAN id (VLAN 1 is the default LAN).
func (c *controller) networkID(vid int) (string, error) {
	nets, err := c.networks()
	if err != nil {
		return "", err
	}
	for _, n := range nets {
		if vlanOf(n) == vid && (vid != 1 || n["purpose"] != "vlan-only") {
			return n["_id"].(string), nil
		}
	}
	return "", fmt.Errorf("no network with VLAN %d; create it with: network NAME %d", vid, vid)
}

func (c *controller) createNetwork(name, vlan string) error {
	vid, err := strconv.Atoi(vlan)
	if err != nil {
		return err
	}
	nets, err := c.networks()
	if err != nil {
		return err
	}
	for _, n := range nets {
		if v, ok := n["vlan"].(float64); ok && int(v) == vid {
			fmt.Printf("network %s VLAN %d present\n", name, vid)
			return nil
		}
	}
	body := map[string]any{"name": name, "purpose": "vlan-only", "vlan_enabled": true, "vlan": vid}
	if err := c.call("POST", "/rest/networkconf", body, nil); err != nil {
		return err
	}
	fmt.Printf("network %s VLAN %d present\n", name, vid)
	return nil
}

func (c *controller) deleteNetwork(vlan string) error {
	vid, err := strconv.Atoi(vlan)
	if err != nil {
		return err
	}
	id, err := c.networkID(vid)
	if err != nil {
		return err
	}
	if err := c.call("DELETE", "/rest/networkconf/"+id, nil, nil); err != nil {
		return err
	}
	fmt.Printf("network with VLAN %d deleted\n", vid)
	return nil
}

// setPort translates port-editor intents into a port_overrides entry.
func (c *controller) setPort(mac, port string, args []string) error {
	opts := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("bad option %q (want key=value)", a)
		}
		opts[k] = v
	}
	o := map[string]any{"setting_preference": "manual"}
	if opts["state"] == "disabled" {
		o["forward"], o["native_networkconf_id"], o["tagged_vlan_mgmt"] = "disabled", "", "block_all"
	} else if err := c.vlanOverride(o, opts); err != nil {
		return err
	}
	if speed := opts["speed"]; speed != "" && speed != "auto" {
		mbps, err := strconv.Atoi(speed)
		if err != nil {
			return err
		}
		o["autoneg"], o["speed"], o["full_duplex"] = false, mbps, opts["duplex"] != "half"
	} else {
		o["autoneg"] = true
	}
	if name, ok := opts["name"]; ok {
		o["name"] = name
	}
	return c.setOverride(mac, port, o)
}

func (c *controller) vlanOverride(o map[string]any, opts map[string]string) error {
	native := 1
	if v, ok := opts["native"]; ok {
		var err error
		if native, err = strconv.Atoi(v); err != nil {
			return err
		}
	}
	id, err := c.networkID(native)
	if err != nil {
		return err
	}
	o["native_networkconf_id"] = id
	switch tagged := opts["tagged"]; tagged {
	case "", "all":
		o["tagged_vlan_mgmt"] = "auto"
	case "none":
		o["tagged_vlan_mgmt"] = "block_all"
	default:
		keep := map[string]bool{}
		for v := range strings.SplitSeq(tagged, ",") {
			vid, err := strconv.Atoi(v)
			if err != nil {
				return err
			}
			nid, err := c.networkID(vid)
			if err != nil {
				return err
			}
			keep[nid] = true
		}
		nets, err := c.networks()
		if err != nil {
			return err
		}
		excluded := []string{}
		for _, n := range nets {
			if id, _ := n["_id"].(string); !keep[id] && n["vlan"] != nil {
				excluded = append(excluded, id)
			}
		}
		o["tagged_vlan_mgmt"], o["excluded_networkconf_ids"] = "custom", excluded
	}
	return nil
}

func (c *controller) setOverride(mac, port string, override map[string]any) error {
	idx, err := strconv.Atoi(port)
	if err != nil {
		return err
	}
	d, err := c.device(mac)
	if err != nil {
		return err
	}
	overrides := []any{}
	existing, _ := d["port_overrides"].([]any)
	for _, e := range existing {
		if m, ok := e.(map[string]any); ok && int(m["port_idx"].(float64)) != idx {
			overrides = append(overrides, m)
		}
	}
	if override != nil {
		override["port_idx"] = idx
		overrides = append(overrides, override)
	}
	if err := c.call("PUT", "/rest/device/"+d.str("_id"), map[string]any{"port_overrides": overrides}, nil); err != nil {
		return err
	}
	if override == nil {
		fmt.Printf("port %d of %s reset\n", idx, mac)
	} else {
		fmt.Printf("port %d of %s updated\n", idx, mac)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
