package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Panourile modulelor sunt servite sub /m/<id>/... pe portul suitei.
// Suita face login automat în fiecare modul și ascunde cookie-urile lor de browser.

var proxyTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	DisableCompression:    true,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       60 * time.Second,
	ResponseHeaderTimeout: 0,
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func isNavigation(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" {
		return m == "navigate"
	}
	return r.Method == "GET" && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// moduleFromReferer – cerere absolută (/api/..) făcută dintr-o pagină /m/<id>/
func moduleFromReferer(r *http.Request) *Module {
	ref := r.Referer()
	if ref == "" {
		return nil
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Host != "" && u.Host != r.Host) {
		return nil
	}
	if !strings.HasPrefix(u.Path, "/m/") {
		return nil
	}
	id := strings.SplitN(strings.TrimPrefix(u.Path, "/m/"), "/", 2)[0]
	return modByID(id)
}

func inList(p string, l []string) bool {
	for _, x := range l {
		if p == x {
			return true
		}
	}
	return false
}

func (m *Module) prefix() string { return "/m/" + m.ID }

func serveModule(w http.ResponseWriter, r *http.Request, m *Module, sub string) {
	if sub == "" {
		sub = "/"
	}
	nav := isNavigation(r)
	pre := m.prefix()

	// paginile de login/logout ale modulului nu mai sunt necesare
	if inList(sub, m.LogoutPaths) {
		if nav {
			http.Redirect(w, r, pre+m.Entry, http.StatusFound)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		}
		return
	}
	if r.Method == "GET" && nav && inList(sub, m.LoginPaths) {
		http.Redirect(w, r, pre+m.Entry, http.StatusFound)
		return
	}

	if f := m.Fake[r.Method+" "+sub]; f != nil {
		code, v := f()
		if s, ok := v.(string); ok {
			http.Error(w, s, code)
		} else {
			writeJSON(w, code, v)
		}
		return
	}

	p := procs[m.ID]
	p.mu.Lock()
	running := p.pid != 0
	p.mu.Unlock()
	if !running {
		if nav {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, offlinePage, html.EscapeString(m.Name), html.EscapeString(m.Name), m.ID)
		} else {
			http.Error(w, m.Name+" este oprit", http.StatusServiceUnavailable)
		}
		return
	}
	if err := m.ensureLogin(false); err != nil {
		http.Error(w, "nu mă pot autentifica în "+m.Name+": "+err.Error()+" (încerc să refac configurația)", http.StatusBadGateway)
		return
	}

	// corpul cererii: păstrat în memorie (pentru reîncercare după re-login) dacă e mic
	var body []byte
	canRetry := true
	if r.Body != nil && r.Method != "GET" && r.Method != "HEAD" {
		if r.ContentLength >= 0 && r.ContentLength <= 4<<20 {
			body, _ = io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
		} else {
			canRetry = false
		}
	}

	target := m.base() + sub
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	build := func() *http.Request {
		var rb io.Reader
		if body != nil {
			rb = bytes.NewReader(body)
		} else if !canRetry {
			rb = r.Body
		}
		out, _ := http.NewRequestWithContext(r.Context(), r.Method, target, rb)
		if body != nil {
			out.ContentLength = int64(len(body))
		} else if !canRetry {
			out.ContentLength = r.ContentLength
		}
		for k, vv := range r.Header {
			out.Header[k] = vv
		}
		for _, h := range hopHeaders {
			out.Header.Del(h)
		}
		out.Header.Del("Cookie")
		out.Header.Del("Authorization")
		out.Header.Del("Accept-Encoding")
		out.Header.Del("X-Forwarded-For")
		out.Header.Del("X-Real-IP")
		out.Header.Del("Forwarded")
		out.Host = "127.0.0.1:" + m.activePort()
		origin := "http://" + out.Host
		if out.Header.Get("Origin") != "" {
			out.Header.Set("Origin", origin)
		}
		if ref := r.Referer(); ref != "" {
			if u, err := url.Parse(ref); err == nil {
				pth := strings.TrimPrefix(u.Path, pre)
				if pth == "" {
					pth = "/"
				}
				out.Header.Set("Referer", origin+pth)
			}
		}
		if !m.NoXFF {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			out.Header.Set("X-Forwarded-For", ip)
			out.Header.Set("X-Real-IP", ip)
		}
		if ck := m.cookieHeader(); ck != "" {
			out.Header.Set("Cookie", ck)
		}
		if m.Basic {
			out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(internalUser+":"+getCfg().InternalPass)))
		}
		return out
	}

	resp, err := proxyTransport.RoundTrip(build())
	if err != nil {
		http.Error(w, m.Name+": "+err.Error(), http.StatusBadGateway)
		return
	}
	if needsLogin(m, resp) {
		if m.Basic {
			m.authProblem()
		} else if canRetry {
			resp.Body.Close()
			if err := m.ensureLogin(true); err != nil {
				http.Error(w, "nu mă pot autentifica în "+m.Name+": "+err.Error(), http.StatusBadGateway)
				return
			}
			resp, err = proxyTransport.RoundTrip(build())
			if err != nil {
				http.Error(w, m.Name+": "+err.Error(), http.StatusBadGateway)
				return
			}
		}
	}
	defer resp.Body.Close()

	m.storeCookies(resp.Cookies())
	h := w.Header()
	for k, vv := range resp.Header {
		h[k] = vv
	}
	for _, x := range hopHeaders {
		h.Del(x)
	}
	h.Del("Set-Cookie")
	h.Del("X-Frame-Options")
	h.Del("Content-Security-Policy")
	h.Set("Referrer-Policy", "same-origin")
	if loc := resp.Header.Get("Location"); loc != "" {
		h.Set("Location", rewriteLocation(m, loc))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if patch := m.JSONPatch[sub]; patch != nil && strings.Contains(ct, "json") && resp.StatusCode == 200 {
		var v map[string]any
		if b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)); err == nil && json.Unmarshal(b, &v) == nil {
			for k, x := range patch {
				v[k] = x
			}
			out, _ := json.Marshal(v)
			h.Del("Content-Length")
			h.Set("Content-Length", fmt.Sprint(len(out)))
			w.WriteHeader(resp.StatusCode)
			w.Write(out)
			return
		}
	}
	isHTML := strings.Contains(ct, "text/html")
	isJS := strings.Contains(ct, "javascript")
	if (isHTML || isJS) && resp.ContentLength < 8<<20 && r.Method != "HEAD" {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err == nil {
			b = rewriteBody(m, b, isHTML)
			h.Del("Content-Length")
			h.Set("Content-Length", fmt.Sprint(len(b)))
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(resp.StatusCode)
			w.Write(b)
			return
		}
	}
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

func needsLogin(m *Module, resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if u, err := url.Parse(resp.Header.Get("Location")); err == nil && inList(u.Path, m.LoginPaths) {
			return true
		}
	}
	return false
}

func rewriteLocation(m *Module, loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.Host != "" {
		if !strings.HasPrefix(u.Host, "127.0.0.1") && !strings.HasPrefix(u.Host, "localhost") {
			return loc
		}
		u.Scheme, u.Host = "", ""
	}
	if strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, m.prefix()+"/") {
		if inList(u.Path, m.LoginPaths) {
			u.Path, u.RawQuery = m.Entry, ""
		}
		u.Path = m.prefix() + u.Path
	}
	return u.String()
}

var (
	reAttr     = regexp.MustCompile(`(\s(?:href|src|action)\s*=\s*["'])/([^/"'])`)
	reAttrRoot = regexp.MustCompile(`(\s(?:href|src|action)\s*=\s*["'])/(["'])`)
	reHead     = regexp.MustCompile(`(?i)<head[^>]*>`)
)

const patchJS = `<script>(function(){var P=%q;
function fx(u){if(typeof u==="string"&&u.charAt(0)==="/"&&u.charAt(1)!=="/"&&u.indexOf(P+"/")!==0&&u!==P)return P+u;return u}
var of=window.fetch;if(of)window.fetch=function(u,o){if(typeof u==="string")u=fx(u);return of.call(this,u,o)};
var xo=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){if(typeof u==="string")arguments[1]=fx(u);return xo.apply(this,arguments)};
if(window.EventSource){var E=window.EventSource;var NE=function(u,c){return new E(fx(u),c)};NE.prototype=E.prototype;window.EventSource=NE}
var wo=window.open;window.open=function(u){if(typeof u==="string")arguments[0]=fx(u);return wo.apply(window,arguments)};
})();</script>`

func rewriteBody(m *Module, b []byte, isHTML bool) []byte {
	pre := m.prefix()
	s := string(b)
	s = reAttr.ReplaceAllString(s, "${1}"+pre+"/${2}")
	s = reAttrRoot.ReplaceAllString(s, "${1}"+pre+"/${2}")
	// ProxyHub: după schimbarea portului, rămâi în suită (nu sări direct pe portul modulului)
	s = strings.ReplaceAll(s, "location.href=location.protocol+'//'+location.hostname+':'+port+'/panel'", "location.href='"+pre+"/panel'")
	if m.originPort != nil {
		// linkurile de stream afișate în panou trebuie să arate portul modulului, nu 8090
		s = strings.ReplaceAll(s, "location.origin", "(location.protocol+'//'+location.hostname+':"+m.originPort(m)+"')")
	}
	if isHTML {
		inj := fmt.Sprintf(patchJS, pre)
		if loc := reHead.FindStringIndex(s); loc != nil {
			s = s[:loc[1]] + inj + s[loc[1]:]
		} else {
			s = inj + s
		}
	}
	return []byte(s)
}

func flushCopy(w http.ResponseWriter, r io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

const offlinePage = `<!doctype html><html><head><meta charset="utf-8"><title>%s</title>
<style>body{margin:0;height:100vh;display:grid;place-items:center;background:#0b0f14;color:#c9d4e0;font:15px system-ui,sans-serif}
.b{text-align:center}.b h2{margin:0 0 8px;color:#fff;font-weight:600}.b p{color:#7d8b9a;margin:0 0 18px}
button{background:#22c55e;color:#04130a;border:0;border-radius:8px;padding:10px 18px;font-weight:600;cursor:pointer}</style></head>
<body><div class="b"><h2>%s este oprit</h2><p>Pornește modulul ca să vezi panoul.</p>
<button onclick="fetch('/suite/api/module/%s/start',{method:'POST'}).then(()=>setTimeout(()=>location.reload(),1500))">Pornește</button></div></body></html>`
