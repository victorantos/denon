// Package dnsserver intercepts vTuner-flavored hostnames and forwards
// everything else to an upstream resolver. It runs on UDP port 53, which
// requires root on Unix.
package dnsserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// DefaultInterceptDomains are hostname suffixes vTuner-style firmwares query.
// radiodenon.com / radiomarantz.com are used by older Denon/Marantz models
// (e.g. AVR-X3000, ~2013) for the user-added-stations path that's separate
// from the main *.vtuner.com directory.
var DefaultInterceptDomains = []string{
	"vtuner.com",
	"radiosetup.com",
	"radiodenon.com",
	"radiomarantz.com",
	"my-noxon.net",
}

// Server intercepts certain DNS queries and forwards the rest.
type Server struct {
	Addr             string   // listen address, e.g. ":53"
	InterceptIP      net.IP   // IP we answer A queries with
	InterceptDomains []string // suffix matches (lowercase), e.g. "vtuner.com"
	Upstream         []string // forwarders, e.g. "1.1.1.1:53"
	Logger           *slog.Logger

	mu  sync.Mutex
	udp *dns.Server
}

func (s *Server) ListenAndServe() error {
	if s.InterceptIP == nil {
		return errors.New("dnsserver: InterceptIP is required")
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handle)
	s.mu.Lock()
	s.udp = &dns.Server{Addr: s.Addr, Net: "udp", Handler: mux}
	s.mu.Unlock()
	return s.udp.ListenAndServe()
}

func (s *Server) Shutdown() error {
	s.mu.Lock()
	srv := s.udp
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.ShutdownContext(ctx)
}

func (s *Server) handle(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		s.fail(w, r)
		return
	}
	q := r.Question[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	if s.shouldIntercept(name) && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA) {
		s.intercept(w, r, q)
		return
	}
	s.forward(w, r)
}

func (s *Server) shouldIntercept(name string) bool {
	for _, suffix := range s.InterceptDomains {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

func (s *Server) intercept(w dns.ResponseWriter, r *dns.Msg, q dns.Question) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if q.Qtype == dns.TypeA {
		if v4 := s.InterceptIP.To4(); v4 != nil {
			m.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   v4,
			}}
		}
	}
	// AAAA queries: empty NOERROR (no Answer section) signals "no v6 record"
	// without forcing the resolver to retry — NXDOMAIN would be wrong since
	// we *do* have an A record.
	s.Logger.Info("dns intercept", "name", q.Name, "qtype", dns.TypeToString[q.Qtype])
	_ = w.WriteMsg(m)
}

func (s *Server) forward(w dns.ResponseWriter, r *dns.Msg) {
	if len(s.Upstream) == 0 {
		s.fail(w, r)
		return
	}
	c := &dns.Client{Net: "udp", Timeout: 4 * time.Second}
	for _, u := range s.Upstream {
		resp, _, err := c.Exchange(r, u)
		if err != nil {
			s.Logger.Debug("upstream try failed", "upstream", u, "err", err)
			continue
		}
		_ = w.WriteMsg(resp)
		return
	}
	s.Logger.Error("all dns upstreams failed", "question", r.Question[0].Name)
	s.fail(w, r)
}

func (s *Server) fail(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}
