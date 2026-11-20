package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// listenerCfg es el resultado de procesar la configuración para una dirección.
type listenerCfg struct {
	addr string
	tls  bool
	mux  *mux
}

func normalizeAddr(a string) string {
	if !strings.Contains(a, ":") {
		return ":" + a
	}
	return a
}

type config struct {
	http   map[string]*listenerCfg
	stream map[string]*streamCfg
}

func buildConfig(path string) (*config, error) {
	top, err := ParseConfig(path)
	if err != nil {
		return nil, err
	}
	cfg := &config{http: map[string]*listenerCfg{}, stream: map[string]*streamCfg{}}
	for _, d := range top {
		if !d.HasBlock {
			continue
		}
		switch d.Name {
		case "http":
			h, err := buildHTTP(d)
			if err != nil {
				return nil, err
			}
			for k, v := range h {
				cfg.http[k] = v
			}
		case "stream":
			s, err := buildStream(d)
			if err != nil {
				return nil, err
			}
			for k, v := range s {
				if _, dup := cfg.stream[k]; dup {
					return nil, fmt.Errorf("stream: %s duplicado", k)
				}
				cfg.stream[k] = v
			}
		}
	}
	if len(cfg.http) == 0 && len(cfg.stream) == 0 {
		return nil, errors.New("falta un bloque http { } o stream { } con al menos un server")
	}
	return cfg, nil
}

func buildHTTP(httpBlock *Directive) (map[string]*listenerCfg, error) {
	httpSc := &scope{dirs: httpBlock.Block}

	ups := map[string]*upstream{}
	for _, d := range httpBlock.Block {
		if d.Name == "upstream" && d.HasBlock && len(d.Args) == 1 {
			u, err := buildUpstream(d)
			if err != nil {
				return nil, err
			}
			ups[d.Args[0]] = u
		}
	}

	out := map[string]*listenerCfg{}
	for _, d := range httpBlock.Block {
		if d.Name != "server" || !d.HasBlock {
			continue
		}
		sc := &scope{dirs: d.Block, parent: httpSc}
		v := &vhost{sc: sc}
		var listens [][]string
		for _, c := range d.Block {
			switch c.Name {
			case "listen":
				if len(c.Args) == 0 {
					return nil, fmt.Errorf("línea %d: listen sin argumentos", c.Line)
				}
				listens = append(listens, c.Args)
			case "server_name":
				v.names = append(v.names, c.Args...)
			case "location":
				l, err := newLocation(c, sc, ups)
				if err != nil {
					return nil, err
				}
				v.locs = append(v.locs, l)
			}
		}
		if len(listens) == 0 {
			listens = [][]string{{"8080"}}
		}
		certD, keyD := sc.first("ssl_certificate"), sc.first("ssl_certificate_key")
		if certD != nil && keyD != nil && len(certD.Args) == 1 && len(keyD.Args) == 1 {
			c, err := tls.LoadX509KeyPair(certD.Args[0], keyD.Args[0])
			if err != nil {
				return nil, fmt.Errorf("línea %d: %v", certD.Line, err)
			}
			v.cert = &c
		}
		for _, la := range listens {
			addr := normalizeAddr(la[0])
			lc := out[addr]
			if lc == nil {
				lc = &listenerCfg{addr: addr, mux: &mux{}}
				out[addr] = lc
			}
			isTLS, isDef := false, false
			for _, f := range la[1:] {
				switch f {
				case "ssl":
					isTLS = true
				case "default_server":
					isDef = true
				}
			}
			if isTLS {
				if v.cert == nil {
					return nil, fmt.Errorf("línea %d: listen ssl requiere ssl_certificate y ssl_certificate_key", d.Line)
				}
				lc.tls = true
			}
			lc.mux.vhosts = append(lc.mux.vhosts, v)
			if isDef || lc.mux.def == nil {
				lc.mux.def = v
			}
		}
	}
	return out, nil
}

// ---------- ciclo de vida ----------

type listener struct {
	cfg *atomic.Pointer[listenerCfg]
	srv *http.Server
	tls bool
}

type app struct {
	conf string
	mu   sync.Mutex
	ls   map[string]*listener
	ss   map[string]*streamListener
}

func (a *app) applyStream(cfgs map[string]*streamCfg) error {
	for key, s := range a.ss {
		if _, ok := cfgs[key]; !ok {
			s.close()
			delete(a.ss, key)
			log.Printf("stream cerrado %s", key)
		}
	}
	for key, c := range cfgs {
		if s, ok := a.ss[key]; ok {
			s.cfg.Store(c)
			continue
		}
		s, err := startStream(c)
		if err != nil {
			return err
		}
		a.ss[key] = s
		log.Printf("stream escuchando en %s", key)
	}
	return nil
}

func (a *app) apply(c *config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.applyStream(c.stream); err != nil {
		return err
	}
	return a.applyHTTP(c.http)
}

func (a *app) applyHTTP(cfgs map[string]*listenerCfg) error {

	// Quitar listeners que ya no existen.
	for addr, l := range a.ls {
		if _, ok := cfgs[addr]; !ok {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			l.srv.Shutdown(ctx)
			cancel()
			delete(a.ls, addr)
			log.Printf("cerrado %s", addr)
		}
	}
	for addr, c := range cfgs {
		if l, ok := a.ls[addr]; ok {
			if l.tls != c.tls {
				log.Printf("aviso: %s cambió ssl on/off; requiere reinicio completo", addr)
			}
			l.cfg.Store(c)
			continue
		}
		holder := &atomic.Pointer[listenerCfg]{}
		holder.Store(c)
		srv := &http.Server{
			Addr: addr,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				holder.Load().mux.ServeHTTP(w, r)
			}),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       75 * time.Second,
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		if c.tls {
			srv.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"h2", "http/1.1"},
				GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
					m := holder.Load().mux
					if v := m.pick(h.ServerName); v != nil && v.cert != nil {
						return v.cert, nil
					}
					return nil, errors.New("sin certificado para " + h.ServerName)
				},
			}
			go func() {
				if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("%s: %v", addr, err)
				}
			}()
		} else {
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("%s: %v", addr, err)
				}
			}()
		}
		a.ls[addr] = &listener{cfg: holder, srv: srv, tls: c.tls}
		scheme := "http"
		if c.tls {
			scheme = "https"
		}
		log.Printf("escuchando en %s (%s)", addr, scheme)
	}
	return nil
}

func main() {
	conf := flag.String("c", "nginx.conf", "fichero de configuración")
	test := flag.Bool("t", false, "comprobar la configuración y salir")
	flag.Parse()

	cfgs, err := buildConfig(*conf)
	if err != nil {
		log.Fatalf("configuración inválida: %v", err)
	}
	if *test {
		fmt.Printf("%s: la sintaxis es correcta\n", *conf)
		return
	}

	a := &app{conf: *conf, ls: map[string]*listener{}, ss: map[string]*streamListener{}}
	if err := a.apply(cfgs); err != nil {
		log.Fatal(err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for s := range sig {
		if s == syscall.SIGHUP {
			cfgs, err := buildConfig(a.conf)
			if err != nil {
				log.Printf("recarga rechazada, se mantiene la configuración anterior: %v", err)
				continue
			}
			if err := a.apply(cfgs); err != nil {
				log.Printf("error al aplicar la recarga: %v", err)
				continue
			}
			log.Printf("configuración recargada")
			continue
		}
		log.Printf("apagando...")
		a.mu.Lock()
		for _, s := range a.ss {
			s.close()
		}
		for _, l := range a.ls {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			l.srv.Shutdown(ctx)
			cancel()
		}
		a.mu.Unlock()
		return
	}
}
