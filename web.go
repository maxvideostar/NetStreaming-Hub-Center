package main

import (
	"embed"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var startedAt = time.Now()

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func authed(r *http.Request) bool {
	c, err := r.Cookie(sessCookie)
	return err == nil && checkSession(c.Value)
}

// ---- limitare încercări login ----
var (
	failMu sync.Mutex
	fails  = map[string]*struct {
		n     int
		until time.Time
	}{}
)

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func locked(ip string) bool {
	failMu.Lock()
	defer failMu.Unlock()
	f := fails[ip]
	return f != nil && time.Now().Before(f.until)
}

func failed(ip string) {
	failMu.Lock()
	defer failMu.Unlock()
	f := fails[ip]
	if f == nil {
		f = &struct {
			n     int
			until time.Time
		}{}
		fails[ip] = f
	}
	f.n++
	if f.n >= 5 {
		f.until, f.n = time.Now().Add(5*time.Minute), 0
	}
}

func handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/m/"):
			rest := strings.TrimPrefix(p, "/m/")
			id, sub, _ := strings.Cut(rest, "/")
			m := modByID(id)
			if m == nil {
				http.NotFound(w, r)
				return
			}
			if !authed(r) {
				deny(w, r)
				return
			}
			if sub == "" && !strings.HasSuffix(p, "/") {
				http.Redirect(w, r, m.prefix()+m.Entry, http.StatusFound)
				return
			}
			serveModule(w, r, m, "/"+sub)
		case strings.HasPrefix(p, "/suite/"):
			suiteRoutes(w, r)
		case moduleFromReferer(r) != nil:
			m := moduleFromReferer(r)
			{
				if !authed(r) {
					deny(w, r)
					return
				}
				if isNavigation(r) || (r.Method == "GET" && p == "/") {
					u := m.prefix() + p
					if r.URL.RawQuery != "" {
						u += "?" + r.URL.RawQuery
					}
					code := http.StatusFound
					if r.Method != "GET" && r.Method != "HEAD" {
						code = http.StatusTemporaryRedirect
					}
					http.Redirect(w, r, u, code)
					return
				}
				serveModule(w, r, m, p)
			}
		case p == "/login":
			handleLogin(w, r)
		case p == "/logout":
			http.SetCookie(w, &http.Cookie{Name: sessCookie, Value: "", Path: "/", MaxAge: -1})
			http.Redirect(w, r, "/login", http.StatusFound)
		case p == "/" || p == "/index.html":
			if !authed(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			serveAsset(w, "web/index.html")
		case p == "/favicon.ico" || p == "/favicon.svg":
			serveAsset(w, "web/favicon.svg")
		default:
			http.NotFound(w, r)
		}
	})
}

func deny(w http.ResponseWriter, r *http.Request) {
	if isNavigation(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.Error(w, "neautentificat", http.StatusUnauthorized)
}

func serveAsset(w http.ResponseWriter, name string) {
	b, err := webFS.ReadFile(name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Write(b)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if authed(r) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		serveAsset(w, "web/login.html")
		return
	}
	ip := remoteIP(r)
	if locked(ip) {
		http.Redirect(w, r, "/login?e=locked", http.StatusFound)
		return
	}
	r.ParseForm()
	u, pw := r.PostFormValue("username"), r.PostFormValue("password")
	c := getCfg()
	if u != c.Username || !checkPassword(c.PassHash, pw) {
		failed(ip)
		log.Printf("login eșuat pentru %q de la %s", u, ip)
		time.Sleep(700 * time.Millisecond)
		http.Redirect(w, r, "/login?e=bad", http.StatusFound)
		return
	}
	log.Printf("login %s de la %s", u, ip)
	http.SetCookie(w, &http.Cookie{Name: sessCookie, Value: makeSession(u), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessTTL.Seconds()), Secure: r.TLS != nil})
	http.Redirect(w, r, "/", http.StatusFound)
}

func suiteRoutes(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		deny(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/suite/api/")
	if r.Method != "GET" {
		if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "//"+r.Host) {
			writeJSON(w, 403, map[string]string{"error": "origine invalidă"})
			return
		}
	}
	switch {
	case p == "status":
		apiStatus(w)
	case p == "password" && r.Method == "POST":
		var in struct{ Old, New, User string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		c := getCfg()
		if !checkPassword(c.PassHash, in.Old) {
			writeJSON(w, 400, map[string]string{"error": "parola actuală e greșită"})
			return
		}
		if len(in.New) < 6 {
			writeJSON(w, 400, map[string]string{"error": "parola nouă: minim 6 caractere"})
			return
		}
		in.User = strings.TrimSpace(in.User)
		updateCfg(func(c *SuiteCfg) {
			c.PassHash = hashPassword(in.New)
			if in.User != "" {
				c.Username = in.User
			}
		})
		u := getCfg().Username
		http.SetCookie(w, &http.Cookie{Name: sessCookie, Value: makeSession(u), Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, MaxAge: int(sessTTL.Seconds())})
		writeJSON(w, 200, map[string]any{"ok": true})
	case p == "settings" && r.Method == "POST":
		var in struct{ Listen string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		port := portOf(in.Listen, "")
		if port == "" {
			writeJSON(w, 400, map[string]string{"error": "port invalid"})
			return
		}
		updateCfg(func(c *SuiteCfg) { c.Listen = ":" + port })
		writeJSON(w, 200, map[string]any{"ok": true, "msg": "Salvat. Repornește nTs-Suite ca să asculte pe portul " + port})
	case strings.HasPrefix(p, "module/"):
		parts := strings.Split(strings.TrimPrefix(p, "module/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		m := modByID(parts[0])
		if m == nil {
			http.NotFound(w, r)
			return
		}
		moduleAction(w, r, m, parts[1])
	default:
		http.NotFound(w, r)
	}
}

func apiStatus(w http.ResponseWriter) {
	c := getCfg()
	cpu, used, total := hostStats()
	host, _ := os.Hostname()
	var mods []ProcInfo
	for _, m := range modules {
		mods = append(mods, procs[m.ID].Info(c.Modules[m.ID].Enabled))
	}
	writeJSON(w, 200, map[string]any{
		"version": version, "os": runtime.GOOS + "/" + runtime.GOARCH, "host": host,
		"uptime": int64(time.Since(startedAt).Seconds()), "cpu": cpu, "mem_used": used, "mem_total": total,
		"user": c.Username, "default_pass": checkPassword(c.PassHash, "admin"), "listen": c.Listen,
		"data_dir": dataDir, "modules": mods,
	})
}

func moduleAction(w http.ResponseWriter, r *http.Request, m *Module, act string) {
	p := procs[m.ID]
	ok := func() { writeJSON(w, 200, p.Info(getCfg().Modules[m.ID].Enabled)) }
	switch act {
	case "logs":
		writeJSON(w, 200, map[string]any{"lines": p.logs.Tail(800)})
		return
	case "config":
		if r.Method == "GET" {
			b, err := os.ReadFile(m.CfgPath())
			if err != nil {
				writeJSON(w, 404, map[string]string{"error": "configurația nu există încă (pornește modulul o dată)"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", `attachment; filename="`+m.ID+"-"+m.CfgFile+`"`)
			w.Write(b)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		var v map[string]any
		if err != nil || json.Unmarshal(b, &v) != nil {
			writeJSON(w, 400, map[string]string{"error": "fișierul nu este un JSON valid"})
			return
		}
		was := p.Wanted()
		p.Stop()
		os.MkdirAll(m.Dir(), 0755)
		if old, err := os.ReadFile(m.CfgPath()); err == nil {
			os.WriteFile(m.CfgPath()+".bak-"+time.Now().Format("20060102-150405"), old, 0600)
		}
		if err := writeFileAtomic(m.CfgPath(), b, 0600); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		p.logf("configurație importată (%d octeți)", len(b))
		if was {
			p.Start()
		}
		ok()
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	switch act {
	case "start":
		if err := p.Start(); err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		updateCfg(func(c *SuiteCfg) { c.Modules[m.ID].Enabled = true })
	case "stop":
		updateCfg(func(c *SuiteCfg) { c.Modules[m.ID].Enabled = false })
		p.Stop()
	case "restart":
		p.Restart()
		updateCfg(func(c *SuiteCfg) { c.Modules[m.ID].Enabled = true })
	default:
		http.NotFound(w, r)
		return
	}
	ok()
}
