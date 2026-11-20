package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const serverName = "gonginx/0.1"

// ---------- upstreams ----------

type upstream struct {
	servers []string // host:port, repetidos según weight
	n       atomic.Uint64
}

func (u *upstream) next() string {
	return u.servers[(u.n.Add(1)-1)%uint64(len(u.servers))]
}

func buildUpstream(d *Directive) (*upstream, error) {
	up := &upstream{}
	for _, c := range d.Block {
		if c.Name != "server" || len(c.Args) == 0 {
			continue
		}
		weight, down := 1, false
		for _, a := range c.Args[1:] {
			if a == "down" {
				down = true
			} else if v, ok := strings.CutPrefix(a, "weight="); ok {
				w, err := strconv.Atoi(v)
				if err != nil || w < 1 {
					return nil, fmt.Errorf("línea %d: weight inválido", c.Line)
				}
				weight = w
			}
		}
		if down {
			continue
		}
		for i := 0; i < weight; i++ {
			up.servers = append(up.servers, c.Args[0])
		}
	}
	if len(up.servers) == 0 {
		return nil, fmt.Errorf("línea %d: upstream %q sin servidores", d.Line, strings.Join(d.Args, " "))
	}
	return up, nil
}

// ---------- location ----------

type locKind int

const (
	locPrefix locKind = iota
	locPrefixNoRe // ^~
	locExact      // =
	locRegex      // ~ y ~*
)

type location struct {
	kind  locKind
	path  string
	re    *regexp.Regexp
	sc    *scope
	proxy *httputil.ReverseProxy
}

func newLocation(d *Directive, parent *scope, ups map[string]*upstream) (*location, error) {
	l := &location{sc: &scope{dirs: d.Block, parent: parent}}
	switch len(d.Args) {
	case 1:
		l.kind, l.path = locPrefix, d.Args[0]
	case 2:
		l.path = d.Args[1]
		switch d.Args[0] {
		case "=":
			l.kind = locExact
		case "^~":
			l.kind = locPrefixNoRe
		case "~", "~*":
			l.kind = locRegex
			expr := l.path
			if d.Args[0] == "~*" {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("línea %d: regex inválida: %v", d.Line, err)
			}
			l.re = re
		default:
			return nil, fmt.Errorf("línea %d: modificador de location desconocido %q", d.Line, d.Args[0])
		}
	default:
		return nil, fmt.Errorf("línea %d: location inválida", d.Line)
	}
	for _, c := range d.Block {
		if c.Name == "proxy_pass" {
			if len(c.Args) != 1 {
				return nil, fmt.Errorf("línea %d: proxy_pass requiere 1 argumento", c.Line)
			}
			p, err := newProxy(l, c.Args[0], ups)
			if err != nil {
				return nil, fmt.Errorf("línea %d: %v", c.Line, err)
			}
			l.proxy = p
		}
	}
	return l, nil
}

func newProxy(l *location, raw string, ups map[string]*upstream) (*httputil.ReverseProxy, error) {
	var u *url.URL
	if rest, ok := strings.CutPrefix(raw, "http://unix:"); ok {
		// http://unix:/ruta/socket.sock:/uri
		sock, uri := rest, ""
		if i := strings.LastIndex(rest, ":"); i > 0 {
			sock, uri = rest[:i], rest[i+1:]
		}
		if !strings.HasPrefix(sock, "/") {
			return nil, fmt.Errorf("proxy_pass inválido: %q (la ruta del socket debe ser absoluta)", raw)
		}
		u = &url.URL{Scheme: "http", Host: encodeUnixHost(sock), Path: uri}
	} else {
		var err error
		u, err = url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("proxy_pass inválido: %q", raw)
		}
	}
	up := ups[u.Host]
	hasURI := u.Path != ""
	return &httputil.ReverseProxy{
		Transport: proxyTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			host := u.Host
			if up != nil {
				host = upstreamHost(up.next())
			}
			p := pr.In.URL.Path
			if hasURI && l.kind != locRegex {
				p = u.Path + strings.TrimPrefix(p, l.path)
			}
			pr.Out.URL.Scheme = u.Scheme
			pr.Out.URL.Host = host
			pr.Out.URL.Path = p
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = host
			if isUnixHost(host) {
				pr.Out.Host = "localhost"
			}
			pr.SetXForwarded()
			for _, h := range l.sc.all("proxy_set_header") {
				if len(h.Args) != 2 {
					continue
				}
				val := expand(h.Args[1], pr.In)
				if strings.EqualFold(h.Args[0], "Host") {
					pr.Out.Host = val
				} else {
					pr.Out.Header.Set(h.Args[0], val)
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error: %v", err)
			http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
		},
	}, nil
}

// expand sustituye las variables más comunes de nginx.
func expand(s string, r *http.Request) string {
	if !strings.Contains(s, "$") {
		return s
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	remote, _, _ := net.SplitHostPort(r.RemoteAddr)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.NewReplacer(
		"$request_uri", r.URL.RequestURI(),
		"$remote_addr", remote,
		"$http_host", r.Host,
		"$host", host,
		"$scheme", scheme,
		"$uri", r.URL.Path,
		"$args", r.URL.RawQuery,
	).Replace(s)
}

func (l *location) serve(w http.ResponseWriter, r *http.Request) {
	for _, h := range l.sc.all("add_header") {
		if len(h.Args) >= 2 {
			w.Header().Add(h.Args[0], expand(h.Args[1], r))
		}
	}
	if d := l.sc.first("return"); d != nil && len(d.Args) >= 1 {
		code, err := strconv.Atoi(d.Args[0])
		if err != nil {
			http.Error(w, "500 Internal Server Error", 500)
			return
		}
		if len(d.Args) >= 2 {
			target := expand(d.Args[1], r)
			if code >= 300 && code < 400 {
				http.Redirect(w, r, target, code)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(code)
			fmt.Fprint(w, target)
			return
		}
		w.WriteHeader(code)
		return
	}
	if l.proxy != nil {
		l.proxy.ServeHTTP(w, r)
		return
	}
	l.static(w, r)
}

func (l *location) indexes() []string {
	if d := l.sc.first("index"); d != nil {
		return d.Args
	}
	return []string{"index.html"}
}

func (l *location) static(w http.ResponseWriter, r *http.Request) {
	root := "html"
	if d := l.sc.first("root"); d != nil && len(d.Args) == 1 {
		root = d.Args[0]
	}
	urlPath := path.Clean("/" + r.URL.Path)
	if strings.HasSuffix(r.URL.Path, "/") && urlPath != "/" {
		urlPath += "/"
	}

	if tf := l.sc.first("try_files"); tf != nil && len(tf.Args) >= 2 {
		last := len(tf.Args) - 1
		for _, a := range tf.Args[:last] {
			if l.tryServe(w, r, root, expand(a, r), false) {
				return
			}
		}
		fb := expand(tf.Args[last], r)
		if code, ok := strings.CutPrefix(fb, "="); ok {
			c, _ := strconv.Atoi(code)
			if c == 0 {
				c = 404
			}
			http.Error(w, fmt.Sprintf("%d %s", c, http.StatusText(c)), c)
			return
		}
		if !l.tryServe(w, r, root, fb, false) {
			http.Error(w, "404 Not Found", http.StatusNotFound)
		}
		return
	}

	if !l.tryServe(w, r, root, urlPath, true) {
		http.Error(w, "404 Not Found", http.StatusNotFound)
	}
}

// tryServe sirve urlPath bajo root si existe (fichero, o directorio con index).
func (l *location) tryServe(w http.ResponseWriter, r *http.Request, root, urlPath string, redirectDir bool) bool {
	clean := path.Clean("/" + urlPath)
	full := filepath.Join(root, filepath.FromSlash(clean))
	fi, err := os.Stat(full)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		if !strings.HasSuffix(urlPath, "/") {
			if redirectDir {
				http.Redirect(w, r, clean+"/", http.StatusMovedPermanently)
				return true
			}
			return false
		}
		for _, idx := range l.indexes() {
			if f := filepath.Join(full, idx); fileExists(f) {
				return serveFile(w, r, f)
			}
		}
		return false
	}
	return serveFile(w, r, full)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func serveFile(w http.ResponseWriter, r *http.Request, p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
	return true
}

// ---------- virtual host ----------

type vhost struct {
	names     []string
	sc        *scope
	locs      []*location
	cert      *tls.Certificate
	isDefault bool
}

func (v *vhost) match(p string) *location {
	var best *location
	for _, l := range v.locs {
		switch l.kind {
		case locExact:
			if l.path == p {
				return l
			}
		case locPrefix, locPrefixNoRe:
			if strings.HasPrefix(p, l.path) && (best == nil || len(l.path) > len(best.path)) {
				best = l
			}
		}
	}
	if best != nil && best.kind == locPrefixNoRe {
		return best
	}
	for _, l := range v.locs {
		if l.kind == locRegex && l.re.MatchString(p) {
			return l
		}
	}
	return best
}

func (v *vhost) hasName(host string) int { // 2 exacto, 1 comodín, 0 no
	res := 0
	for _, n := range v.names {
		switch {
		case strings.EqualFold(n, host):
			return 2
		case strings.HasPrefix(n, "*.") && strings.HasSuffix(strings.ToLower(host), strings.ToLower(n[1:])):
			res = 1
		}
	}
	return res
}

// ---------- mux por listener ----------

type mux struct {
	vhosts []*vhost
	def    *vhost
}

func (m *mux) pick(host string) *vhost {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var wild *vhost
	for _, v := range m.vhosts {
		switch v.hasName(host) {
		case 2:
			return v
		case 1:
			if wild == nil {
				wild = v
			}
		}
	}
	if wild != nil {
		return wild
	}
	return m.def
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(c int) {
	if s.status == 0 {
		s.status = c
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = 200
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap permite a http.ResponseController alcanzar Hijack/Flush (WebSockets, SSE).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// normPath limpia ".." y "//" pero conserva la barra final.
func normPath(p string) string {
	c := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

func (m *mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	rec.Header().Set("Server", serverName)

	logOn := true
	v := m.pick(r.Host)
	if v == nil {
		http.Error(rec, "404 Not Found", http.StatusNotFound)
	} else if loc := v.match(normPath(r.URL.Path)); loc == nil {
		http.Error(rec, "404 Not Found", http.StatusNotFound)
		if d := v.sc.first("access_log"); d != nil && len(d.Args) > 0 && d.Args[0] == "off" {
			logOn = false
		}
	} else {
		if d := loc.sc.first("access_log"); d != nil && len(d.Args) > 0 && d.Args[0] == "off" {
			logOn = false
		}
		loc.serve(rec, r)
	}
	if logOn {
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Printf("%s - [%s] \"%s %s %s\" %d %d %.3fms\n",
			remote, start.Format("02/Jan/2006:15:04:05 -0700"),
			r.Method, r.URL.RequestURI(), r.Proto, rec.status, rec.bytes,
			float64(time.Since(start).Microseconds())/1000)
	}
}
