package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// streamCfg describe un `server { }` del bloque stream.
type streamCfg struct {
	addr           string
	udp            bool
	pick           func() string // "host:port" o "unix:/ruta"
	connectTimeout time.Duration
	idleTimeout    time.Duration
}

// parseDuration entiende los sufijos de nginx: ms, s, m, h, d (sin sufijo = segundos).
func parseDuration(s string) (time.Duration, error) {
	units := []struct {
		suf string
		d   time.Duration
	}{{"ms", time.Millisecond}, {"s", time.Second}, {"m", time.Minute}, {"h", time.Hour}, {"d", 24 * time.Hour}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suf); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("duración inválida %q", s)
			}
			return time.Duration(n) * u.d, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("duración inválida %q", s)
	}
	return time.Duration(n) * time.Second, nil
}

func buildStream(blk *Directive) (map[string]*streamCfg, error) {
	ups := map[string]*upstream{}
	for _, d := range blk.Block {
		if d.Name == "upstream" && d.HasBlock && len(d.Args) == 1 {
			u, err := buildUpstream(d)
			if err != nil {
				return nil, err
			}
			ups[d.Args[0]] = u
		}
	}
	out := map[string]*streamCfg{}
	for _, d := range blk.Block {
		if d.Name != "server" || !d.HasBlock {
			continue
		}
		cfg := &streamCfg{connectTimeout: 60 * time.Second, idleTimeout: 10 * time.Minute}
		var listens [][]string
		var target string
		for _, c := range d.Block {
			switch c.Name {
			case "listen":
				if len(c.Args) == 0 {
					return nil, fmt.Errorf("línea %d: listen sin argumentos", c.Line)
				}
				listens = append(listens, c.Args)
			case "proxy_pass":
				if len(c.Args) != 1 {
					return nil, fmt.Errorf("línea %d: proxy_pass requiere 1 argumento", c.Line)
				}
				target = c.Args[0]
			case "proxy_connect_timeout", "proxy_timeout":
				if len(c.Args) != 1 {
					return nil, fmt.Errorf("línea %d: %s requiere 1 argumento", c.Line, c.Name)
				}
				dur, err := parseDuration(c.Args[0])
				if err != nil {
					return nil, fmt.Errorf("línea %d: %v", c.Line, err)
				}
				if c.Name == "proxy_timeout" {
					cfg.idleTimeout = dur
				} else {
					cfg.connectTimeout = dur
				}
			}
		}
		if len(listens) == 0 || target == "" {
			return nil, fmt.Errorf("línea %d: el server de stream necesita listen y proxy_pass", d.Line)
		}
		if up, ok := ups[target]; ok {
			cfg.pick = up.next
		} else {
			t := target
			cfg.pick = func() string { return t }
		}
		for _, la := range listens {
			c := *cfg
			c.addr = normalizeAddr(la[0])
			for _, f := range la[1:] {
				if f == "udp" {
					c.udp = true
				}
			}
			if c.udp && (strings.HasPrefix(target, "unix:")) {
				return nil, fmt.Errorf("línea %d: UDP hacia socket Unix no soportado", d.Line)
			}
			key := "tcp " + c.addr
			if c.udp {
				key = "udp " + c.addr
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("línea %d: %s duplicado", d.Line, key)
			}
			out[key] = &c
		}
	}
	return out, nil
}

// ---------- runtime ----------

type streamListener struct {
	cfg  atomic.Pointer[streamCfg]
	ln   net.Listener   // TCP
	pc   net.PacketConn // UDP
	done chan struct{}
}

func startStream(c *streamCfg) (*streamListener, error) {
	s := &streamListener{done: make(chan struct{})}
	s.cfg.Store(c)
	if c.udp {
		pc, err := net.ListenPacket("udp", c.addr)
		if err != nil {
			return nil, err
		}
		s.pc = pc
		go s.serveUDP()
	} else {
		ln, err := net.Listen("tcp", c.addr)
		if err != nil {
			return nil, err
		}
		s.ln = ln
		go s.serveTCP()
	}
	return s, nil
}

func (s *streamListener) close() {
	close(s.done)
	if s.ln != nil {
		s.ln.Close()
	}
	if s.pc != nil {
		s.pc.Close()
	}
}

// idleConn renueva un deadline en cada lectura/escritura (proxy_timeout).
type idleConn struct {
	net.Conn
	d time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if c.d > 0 {
		c.Conn.SetReadDeadline(time.Now().Add(c.d))
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if c.d > 0 {
		c.Conn.SetWriteDeadline(time.Now().Add(c.d))
	}
	return c.Conn.Write(b)
}

func (s *streamListener) serveTCP() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("stream accept: %v", err)
			}
			return
		}
		go s.handleTCP(c)
	}
}

func (s *streamListener) handleTCP(client net.Conn) {
	cfg := s.cfg.Load()
	target := cfg.pick()
	network, addr := dialTarget(target, "tcp")
	up, err := (&net.Dialer{Timeout: cfg.connectTimeout}).Dial(network, addr)
	if err != nil {
		log.Printf("stream %s -> %s: %v", cfg.addr, target, err)
		client.Close()
		return
	}
	start := time.Now()
	var sent, recv int64
	a, b := &idleConn{client, cfg.idleTimeout}, &idleConn{up, cfg.idleTimeout}
	done := make(chan struct{}, 2)
	go func() { n, _ := io.Copy(b, a); atomic.AddInt64(&sent, n); done <- struct{}{} }()
	go func() { n, _ := io.Copy(a, b); atomic.AddInt64(&recv, n); done <- struct{}{} }()
	<-done // cuando un lado cierra, se cierran ambos (como nginx sin proxy_half_close)
	client.Close()
	up.Close()
	<-done
	remote, _, _ := net.SplitHostPort(client.RemoteAddr().String())
	fmt.Printf("%s [%s] TCP %s -> %s %d/%d bytes %.3fs\n", remote, start.Format("02/Jan/2006:15:04:05 -0700"),
		cfg.addr, target, atomic.LoadInt64(&sent), atomic.LoadInt64(&recv), time.Since(start).Seconds())
}

type udpSession struct {
	up     net.Conn
	active atomic.Int64 // unix nano de la última actividad del cliente
}

func (s *streamListener) serveUDP() {
	var mu sync.Mutex
	sessions := map[string]*udpSession{}
	buf := make([]byte, 65535)
	for {
		n, raddr, err := s.pc.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("stream udp: %v", err)
			}
			return
		}
		cfg := s.cfg.Load()
		key := raddr.String()
		mu.Lock()
		sess := sessions[key]
		if sess == nil {
			network, addr := dialTarget(cfg.pick(), "udp")
			up, err := (&net.Dialer{Timeout: cfg.connectTimeout}).Dial(network, addr)
			if err != nil {
				mu.Unlock()
				log.Printf("stream udp %s -> %s: %v", cfg.addr, addr, err)
				continue
			}
			sess = &udpSession{up: up}
			sessions[key] = sess
			go func(sess *udpSession, raddr net.Addr, key string, idle time.Duration) {
				defer func() {
					mu.Lock()
					delete(sessions, key)
					mu.Unlock()
					sess.up.Close()
				}()
				rb := make([]byte, 65535)
				for {
					sess.up.SetReadDeadline(time.Now().Add(idle))
					n, err := sess.up.Read(rb)
					if err != nil {
						var ne net.Error
						if errors.As(err, &ne) && ne.Timeout() &&
							time.Since(time.Unix(0, sess.active.Load())) < idle {
							continue // el cliente sigue activo
						}
						return
					}
					if _, err := s.pc.WriteTo(rb[:n], raddr); err != nil {
						return
					}
				}
			}(sess, raddr, key, cfg.idleTimeout)
		}
		mu.Unlock()
		sess.active.Store(time.Now().UnixNano())
		sess.up.Write(buf[:n])
	}
}
