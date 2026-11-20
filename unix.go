package main

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"
)

// Los sockets Unix se codifican como un "host" ficticio (unix-<hex de la ruta>)
// para poder pasarlos por net/http, y el DialContext del transporte los decodifica.
const unixHostPrefix = "unix-"

func encodeUnixHost(sock string) string { return unixHostPrefix + hex.EncodeToString([]byte(sock)) }

func isUnixHost(h string) bool { return strings.HasPrefix(h, unixHostPrefix) }

// upstreamHost convierte "unix:/ruta" en el host ficticio; deja intactos host:port.
func upstreamHost(server string) string {
	if p, ok := strings.CutPrefix(server, "unix:"); ok {
		return encodeUnixHost(p)
	}
	return server
}

// dialTarget devuelve (network, address) para net.Dial a partir de "host:port" (con defNet: tcp/udp) o "unix:/ruta".
func dialTarget(server, defNet string) (string, string) {
	if p, ok := strings.CutPrefix(server, "unix:"); ok {
		return "unix", p
	}
	return defNet, server
}

var proxyTransport = func() *http.Transport {
	d := &net.Dialer{Timeout: 60 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, _, err := net.SplitHostPort(addr); err == nil && isUnixHost(host) {
				if b, err := hex.DecodeString(strings.TrimPrefix(host, unixHostPrefix)); err == nil {
					return d.DialContext(ctx, "unix", string(b))
				}
			}
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
}()
