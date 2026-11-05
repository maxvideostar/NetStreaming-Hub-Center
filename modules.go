package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const internalUser = "nts-suite"

// Module descrie una din aplicațiile incluse.
type Module struct {
	ID, Name, Desc string
	Bin            string // nume executabil (fără .exe)
	CfgFile        string
	Entry          string   // pagina principală a panoului
	LoginPaths     []string // navigări spre acestea -> redirect la Entry (suita face login singură)
	LogoutPaths    []string
	NoXFF          bool // nu trimite X-Forwarded-For (VideoEncoder: acces local fără parolă)
	Basic          bool // autentificare HTTP Basic (nTs-relay)

	// "serviciul" modulului este suita: răspunsuri înlocuite + câmpuri JSON forțate
	Fake      map[string]func() (int, any) // "METODĂ /cale" -> răspuns
	JSONPatch map[string]map[string]any    // "/cale" -> câmpuri suprascrise în răspunsul JSON

	prepare     func(m *Module) error
	args        func(m *Module) []string
	env         []string
	panelPort   func(m *Module) string   // portul panoului (pe 127.0.0.1)
	publicPorts func(m *Module) []string // porturi deschise spre clienți (afișare + conflicte)
	originPort  func(m *Module) string   // pentru rescrierea location.origin în panou
	login       func(m *Module) error

	jarMu    sync.Mutex
	jar      map[string]string
	lastFix  time.Time
	PortKeys []string // câmpurile de port din configurația modulului (pentru revenire la conflict)
	curPort  string   // portul panoului pe care ascultă procesul ACUM (config poate fi deja schimbat)
}

func (m *Module) Dir() string     { return filepath.Join(dataDir, m.ID) }
func (m *Module) CfgPath() string { return filepath.Join(m.Dir(), m.CfgFile) }
func (m *Module) BinPath() string {
	n := m.Bin
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return filepath.Join(dataDir, "bin", n)
}

var modules = []*Module{
	{
		ID: "ntsrelay", Name: "nTs-relay", Bin: "nts-relay", CfgFile: "config.json",
		Desc:  "Relay MPEG-TS tip Astra (UDP/HTTP/HLS/RTMP/SRT → HTTP-TS/HLS/UDP), PID-uri originale",
		Entry: "/", Basic: true,
	},
	{
		ID: "proxyhub", Name: "ProxyHub", Bin: "proxyhub", CfgFile: "proxyhub.json",
		Desc:  "udpxy (multicast → HTTP) + relay canale HTTP-TS / HLS",
		Entry: "/panel", LoginPaths: []string{"/login"}, LogoutPaths: []string{"/logout"},
		env: []string{"PROXYHUB_NOBROWSER=1"},
	},
	{
		ID: "rtmp", Name: "RTMP Media Server", Bin: "rtmp-media-server", CfgFile: "config.json",
		Desc:  "Server RTMP tip Wowza: ingest, HLS/FLV/TS, push, pull, DVR, tokenuri",
		Entry: "/panel/",
	},
	{
		ID: "videoencoder", Name: "VideoEncoder", Bin: "videoencoder", CfgFile: "config.json",
		Desc:  "Encoder FFmpeg (QSV/AMF/x264) – joburi, streaming RTMP, presetări",
		Entry: "/", LoginPaths: []string{"/login"}, LogoutPaths: []string{"/api/logout"}, NoXFF: true,
	},
	{
		ID: "recmartori", Name: "Rec-Martori", Bin: "rec-martori", CfgFile: "config.json",
		Desc:  "Înregistrări martor FFmpeg pe segmente (M9TV, M9 Plus...)",
		Entry: "/", LoginPaths: []string{"/login"}, LogoutPaths: []string{"/logout"},
	},
}

const managedMsg = "Modulul rulează deja ca serviciu prin nTs-Suite – nu e nevoie de un serviciu separat. Pornirea automată se face din suită (Prezentare → Pornește)."

func init() {
	busy := func() (int, any) { return 409, managedMsg }
	rm := modByID("recmartori")
	rm.Fake = map[string]func() (int, any){
		"GET /api/service": func() (int, any) {
			return 200, map[string]any{"supported": true, "is_service": true, "installed": true, "state": "running",
				"admin": true, "last_result": "gestionat de nTs-Suite", "pid": procs["recmartori"].Info(true).PID}
		},
		"POST /api/service/install":   busy,
		"POST /api/service/uninstall": busy,
	}
	rm.JSONPatch = map[string]map[string]any{"/api/status": {"service": true}}

	ve := modByID("videoencoder")
	ve.Fake = map[string]func() (int, any){
		"GET /api/service": func() (int, any) {
			return 200, map[string]any{"supported": true, "state": "running", "is_service": true, "name": "nTs-Suite"}
		},
	}
	for _, a := range []string{"install", "uninstall", "start", "stop", "restart"} {
		ve.Fake["POST /api/service/"+a] = busy
	}
	ve.JSONPatch = map[string]map[string]any{"/api/system": {"is_service": true}}
}

func init() {
	keys := map[string][]string{
		"ntsrelay":     {"listen", "listen_extra"},
		"proxyhub":     {"listen", "udpxy_listen", "udpxy_enabled"},
		"rtmp":         {"http_port", "rtmp_port"},
		"videoencoder": {"port", "bind"},
		"recmartori":   {"listen"},
	}
	for id, k := range keys {
		modByID(id).PortKeys = k
	}
}

func modByID(id string) *Module {
	for _, m := range modules {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// ---------------- helperi JSON ----------------

func readMap(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	// fișier salvat cu Notepad / PowerShell 5 -> BOM UTF-8; modulele (Go) nu-l acceptă
	if bom := []byte{0xEF, 0xBB, 0xBF}; bytes.HasPrefix(b, bom) {
		b = bytes.TrimPrefix(b, bom)
		writeFileAtomic(path, b, 0600)
	}
	m := map[string]any{}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s invalid: %v", filepath.Base(path), err)
	}
	return m, nil
}

func writeMap(path string, m map[string]any) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return writeFileAtomic(path, b, 0600)
}

func str(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		switch t := v.(type) {
		case string:
			return t
		case json.Number:
			return t.String()
		}
	}
	return ""
}

func setIf(m map[string]any, k string, v any, changed *bool) {
	cur, ok := m[k]
	if ok && fmt.Sprint(cur) == fmt.Sprint(v) {
		return
	}
	m[k] = v
	*changed = true
}

func portOf(listen, def string) string {
	if listen == "" {
		return def
	}
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		return p
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(listen, ":")); err == nil {
		return strings.TrimPrefix(listen, ":")
	}
	return def
}

func cfgMap(m *Module) map[string]any {
	mm, err := readMap(m.CfgPath())
	if err != nil {
		return map[string]any{}
	}
	return mm
}

// ---------------- hash-uri compatibile cu fiecare aplicație ----------------

func rmsHash(p string) string { // rtmp-media-server: salt$sha256(salt+pass)
	b := make([]byte, 8)
	rand.Read(b)
	salt := hex.EncodeToString(b)
	h := sha256.Sum256([]byte(salt + p))
	return salt + "$" + hex.EncodeToString(h[:])
}

func rmsCheck(stored, p string) bool {
	parts := strings.SplitN(stored, "$", 2)
	if len(parts) != 2 {
		return false
	}
	h := sha256.Sum256([]byte(parts[0] + p))
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(hex.EncodeToString(h[:]))) == 1
}

// ---------------- configurare per modul ----------------

func init() {
	ip := func() string { return getCfg().InternalPass }

	// nTs-relay
	nr := modByID("ntsrelay")
	nr.args = func(m *Module) []string { return []string{"-config", m.CfgPath()} }
	nr.prepare = func(m *Module) error {
		c, err := readMap(m.CfgPath())
		if err != nil {
			return err
		}
		ch := false
		if len(c) == 0 {
			c = map[string]any{"listen": ":4000", "tokens": []any{}, "hls_segment": 4, "hls_window": 6, "channels": []any{}}
			ch = true
		}
		setIf(c, "admin_user", internalUser, &ch)
		setIf(c, "admin_pass", ip(), &ch)
		if ch {
			return writeMap(m.CfgPath(), c)
		}
		return nil
	}
	nr.panelPort = func(m *Module) string { return portOf(str(cfgMap(m), "listen"), "4000") }
	nr.publicPorts = func(m *Module) []string {
		c := cfgMap(m)
		ps := []string{portOf(str(c, "listen"), "4000")}
		if ex, ok := c["listen_extra"].([]any); ok {
			for _, e := range ex {
				if s, ok := e.(string); ok {
					ps = append(ps, portOf(s, ""))
				}
			}
		}
		return ps
	}
	nr.originPort = nr.panelPort

	// ProxyHub
	ph := modByID("proxyhub")
	ph.args = func(m *Module) []string { return []string{"-c", m.CfgPath()} }
	ph.prepare = func(m *Module) error {
		c, err := readMap(m.CfgPath())
		if err != nil {
			return err
		}
		ch := false
		if len(c) == 0 {
			// 4001 implicit ca să nu se bată cu nTs-relay (:4000); la import se păstrează portul tău
			c = map[string]any{"listen": ":4001", "udpxy_listen": ":7000", "udpxy_enabled": true, "channels": []any{}}
			ch = true
		}
		setIf(c, "username", internalUser, &ch)
		if !checkPassword(str(c, "password_hash"), ip()) {
			c["password_hash"] = hashPassword(ip())
			ch = true
		}
		if ch {
			return writeMap(m.CfgPath(), c)
		}
		return nil
	}
	ph.panelPort = func(m *Module) string { return portOf(str(cfgMap(m), "listen"), "4000") }
	ph.publicPorts = func(m *Module) []string {
		c := cfgMap(m)
		ps := []string{portOf(str(c, "listen"), "4000")}
		if v, ok := c["udpxy_enabled"].(bool); !ok || v {
			ps = append(ps, portOf(str(c, "udpxy_listen"), "7000"))
		}
		return ps
	}
	ph.originPort = ph.panelPort
	ph.login = func(m *Module) error {
		return loginForm(m, "/login", url.Values{"username": {internalUser}, "password": {ip()}})
	}

	// RTMP Media Server
	rs := modByID("rtmp")
	rs.args = func(m *Module) []string { return []string{"-config", m.CfgPath()} }
	rs.prepare = func(m *Module) error {
		c, err := readMap(m.CfgPath())
		if err != nil {
			return err
		}
		ch := false
		setIf(c, "admin_user", internalUser, &ch)
		if !rmsCheck(str(c, "admin_hash"), ip()) {
			c["admin_hash"] = rmsHash(ip())
			ch = true
		}
		if ch {
			return writeMap(m.CfgPath(), c)
		}
		return nil
	}
	rs.panelPort = func(m *Module) string {
		p := str(cfgMap(m), "http_port")
		if p == "" || p == "0" {
			p = "8080"
		}
		return p
	}
	rs.publicPorts = func(m *Module) []string {
		r := str(cfgMap(m), "rtmp_port")
		if r == "" || r == "0" {
			r = "1935"
		}
		return []string{r, rs.panelPort(m)}
	}
	rs.originPort = rs.panelPort
	rs.login = func(m *Module) error {
		return loginJSON(m, "/api/login", map[string]string{"User": internalUser, "Pass": ip()})
	}

	// VideoEncoder – panou doar local, fără parolă de pe 127.0.0.1
	ve := modByID("videoencoder")
	ve.args = func(m *Module) []string { return []string{"-data", m.Dir(), "-nogui"} }
	ve.prepare = func(m *Module) error {
		c, err := readMap(m.CfgPath())
		if err != nil {
			return err
		}
		ch := false
		setIf(c, "port", 18093, &ch)
		setIf(c, "bind", "127.0.0.1", &ch)
		setIf(c, "local_no_auth", true, &ch)
		setIf(c, "open_window", false, &ch)
		if ch {
			return writeMap(m.CfgPath(), c)
		}
		return nil
	}
	ve.panelPort = func(m *Module) string {
		p := str(cfgMap(m), "port")
		if p == "" || p == "0" {
			p = "18093"
		}
		return p
	}
	ve.publicPorts = func(m *Module) []string { return nil }

	// Rec-Martori – panou doar local
	rm := modByID("recmartori")
	rm.args = func(m *Module) []string { return []string{"-config", m.CfgPath()} }
	rm.prepare = func(m *Module) error {
		c, err := readMap(m.CfgPath())
		if err != nil {
			return err
		}
		ch := false
		l := str(c, "listen")
		if !strings.HasPrefix(l, "127.0.0.1:") {
			c["listen"] = "127.0.0.1:18094"
			ch = true
		}
		setIf(c, "user", internalUser, &ch)
		setIf(c, "password", ip(), &ch)
		if ch {
			return writeMap(m.CfgPath(), c)
		}
		return nil
	}
	rm.panelPort = func(m *Module) string { return portOf(str(cfgMap(m), "listen"), "18094") }
	rm.publicPorts = func(m *Module) []string { return nil }
	rm.login = func(m *Module) error {
		return loginForm(m, "/login", url.Values{"user": {internalUser}, "pass": {ip()}})
	}
}

// ---------------- login automat în module ----------------

var loginClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (m *Module) activePort() string {
	m.jarMu.Lock()
	p := m.curPort
	m.jarMu.Unlock()
	if p == "" {
		p = m.panelPort(m)
	}
	return p
}

func (m *Module) setActivePort(p string) {
	m.jarMu.Lock()
	m.curPort = p
	m.jarMu.Unlock()
}

func (m *Module) base() string { return "http://127.0.0.1:" + m.activePort() }

func portsKey(m *Module) string {
	return strings.Join(append(append([]string{}, m.publicPorts(m)...), "panel:"+m.panelPort(m)), ",")
}

func (m *Module) cookieHeader() string {
	m.jarMu.Lock()
	defer m.jarMu.Unlock()
	var p []string
	for k, v := range m.jar {
		p = append(p, k+"="+v)
	}
	return strings.Join(p, "; ")
}

func (m *Module) hasCookies() bool {
	m.jarMu.Lock()
	defer m.jarMu.Unlock()
	return len(m.jar) > 0
}

func (m *Module) clearCookies() {
	m.jarMu.Lock()
	m.jar = nil
	m.jarMu.Unlock()
}

func (m *Module) storeCookies(cs []*http.Cookie) int {
	m.jarMu.Lock()
	defer m.jarMu.Unlock()
	if m.jar == nil {
		m.jar = map[string]string{}
	}
	n := 0
	for _, c := range cs {
		if c.Value == "" || c.MaxAge < 0 {
			delete(m.jar, c.Name)
			continue
		}
		m.jar[c.Name] = c.Value
		n++
	}
	return n
}

func loginForm(m *Module, path string, v url.Values) error {
	req, _ := http.NewRequest("POST", m.base()+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doLogin(m, req)
}

func loginJSON(m *Module, path string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", m.base()+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return doLogin(m, req)
}

func doLogin(m *Module, req *http.Request) error {
	resp, err := loginClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 || m.storeCookies(resp.Cookies()) == 0 {
		return fmt.Errorf("login %s respins (HTTP %d)", m.Name, resp.StatusCode)
	}
	return nil
}

// ensureLogin – login dacă nu avem sesiune.
func (m *Module) ensureLogin(force bool) error {
	if m.login == nil {
		return nil
	}
	if !force && m.hasCookies() {
		return nil
	}
	m.clearCookies()
	err := m.login(m)
	if err != nil {
		m.authProblem()
	}
	return err
}

// authProblem – parola modulului a fost schimbată din panoul lui:
// rescriem configurația și repornim modulul (cel mult o dată pe minut).
func (m *Module) authProblem() {
	m.jarMu.Lock()
	if time.Since(m.lastFix) < time.Minute {
		m.jarMu.Unlock()
		return
	}
	m.lastFix = time.Now()
	m.jarMu.Unlock()
	if p := procs[m.ID]; p != nil && p.Wanted() {
		p.logs.Write([]byte("[suite] autentificare respinsă – refac configurația și repornesc modulul\n"))
		go p.Restart()
	}
}
