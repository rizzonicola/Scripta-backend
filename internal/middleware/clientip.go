package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIPResolver determina l'IP "reale" del client senza permettere lo
// spoofing degli header di inoltro.
//
// PROBLEMA PRECEDENTE: il rate limiter si fidava SEMPRE di Cf-Connecting-IP.
// Ma quell'header è affidabile solo se lo imposta un proxy fidato
// (Cloudflare/cloudflared): un client che raggiunge direttamente il server
// (porta esposta, docker publish, rete interna) può inviarne uno arbitrario
// e ottenere un bucket nuovo ad ogni richiesta, aggirando il rate limit.
//
// REGOLA ORA: gli header di inoltro (Cf-Connecting-IP, X-Forwarded-For) sono
// letti SOLO se la connessione TCP proviene da un indirizzo incluso in
// TRUSTED_PROXIES (elenco di IP/CIDR). Con l'elenco vuoto (default) nessun
// header è creduto e si usa sempre r.RemoteAddr.
type ClientIPResolver struct {
	trusted []netip.Prefix
}

// NewClientIPResolver costruisce il resolver a partire da un elenco di IP o
// CIDR (es. "127.0.0.1", "10.0.0.0/8", "::1").
func NewClientIPResolver(entries []string) (*ClientIPResolver, error) {
	res := &ClientIPResolver{}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("TRUSTED_PROXIES: CIDR non valido %q: %w", e, err)
			}
			res.trusted = append(res.trusted, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: IP non valido %q: %w", e, err)
		}
		a = a.Unmap()
		res.trusted = append(res.trusted, netip.PrefixFrom(a, a.BitLen()))
	}
	return res, nil
}

// ParseTrustedProxies spezza una lista separata da virgole (env var).
func ParseTrustedProxies(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	return strings.Split(csv, ",")
}

func (c *ClientIPResolver) isTrusted(a netip.Addr) bool {
	if c == nil {
		return false
	}
	a = a.Unmap()
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

// ClientIP restituisce l'IP da usare come chiave di rate limiting.
func (c *ClientIPResolver) ClientIP(r *http.Request) string {
	peer, ok := remoteAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	if !c.isTrusted(peer) {
		return peer.String()
	}

	// Il peer è un proxy fidato: si può credere agli header che ha impostato.
	if v := strings.TrimSpace(r.Header.Get("Cf-Connecting-IP")); v != "" {
		if a, err := netip.ParseAddr(v); err == nil {
			return a.Unmap().WithZone("").String()
		}
	}

	// X-Forwarded-For: si scorre da destra (hop più vicino) verso sinistra e
	// si prende il primo indirizzo NON fidato. Le voci più a sinistra sono
	// controllate dal client e non vanno mai credute.
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		v := strings.TrimSpace(parts[i])
		if v == "" {
			continue
		}
		a, err := netip.ParseAddr(v)
		if err != nil {
			break // voce malformata: non proseguire verso sinistra
		}
		a = a.Unmap().WithZone("")
		if !c.isTrusted(a) {
			return a.String()
		}
	}
	return peer.String()
}
