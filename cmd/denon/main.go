package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"denon/internal/dnsserver"
	"denon/internal/radiobrowser"
	"denon/internal/vtuner"
)

const userAgent = "denon/0.1 (vtuner replacement; https://github.com/victorantos/denon)"

func main() {
	var (
		httpAddr  = flag.String("http", ":8080", "HTTP listen address (use :80 in production; requires root)")
		baseURL   = flag.String("base-url", "", "Public base URL receivers see. Auto-detected from the first non-loopback IPv4 if empty.")
		dnsAddr   = flag.String("dns", "", "DNS listen address, e.g. :53. Empty disables DNS (use Pi-hole/router instead). Requires root on Unix.")
		upstream  = flag.String("dns-upstream", "1.1.1.1:53,9.9.9.9:53", "Comma-separated upstream DNS forwarders.")
		intercept = flag.String("dns-intercept", strings.Join(dnsserver.DefaultInterceptDomains, ","), "Comma-separated hostname suffixes to intercept.")
		ifaceIP   = flag.String("intercept-ip", "", "IP returned for intercepted A queries. Defaults to the host portion of --base-url.")
		legacyCC  = flag.String("legacy-countries", "", "Comma-separated ISO country codes to source legacy-favorite fallbacks from (e.g. 'MD,RO'). Empty = global top-voted.")
		legacyEx  = flag.String("legacy-exclude", "", "Comma-separated terms to exclude from legacy-favorite fallback (case-insensitive match against station name + tags). E.g. 'manele,gypsy'.")
		legacySt  = flag.String("legacy-stations", "", "Pipe-separated explicit station names to use as the legacy-favorite pool (overrides --legacy-countries). E.g. 'Radio Moldova|Radio Moldova Tineret|Radio Moldova Muzical'.")
		verbose   = flag.Bool("v", false, "Verbose logging.")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	resolvedBaseURL := *baseURL
	if resolvedBaseURL == "" {
		resolvedBaseURL = autoBaseURL(*httpAddr)
		logger.Info("auto-detected base url", "url", resolvedBaseURL, "hint", "set --base-url explicitly in production")
	}

	httpSrv := &http.Server{
		Addr: *httpAddr,
		Handler: (&vtuner.Server{
			BaseURL:            resolvedBaseURL,
			Logger:             logger,
			Radio:              radiobrowser.NewClient(userAgent),
			LegacyStations:     splitOn("|", *legacySt),
			LegacyCountries:    splitCSV(*legacyCC),
			LegacyExcludeTerms: splitCSV(*legacyEx),
		}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("http listening", "addr", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	var dnsSrv *dnsserver.Server
	if *dnsAddr != "" {
		ip := net.ParseIP(*ifaceIP)
		if ip == nil {
			ip = net.ParseIP(hostFromBaseURL(resolvedBaseURL))
		}
		if ip == nil {
			logger.Error("dns: cannot determine intercept IP; pass --intercept-ip explicitly")
			stop()
			return
		}
		dnsSrv = &dnsserver.Server{
			Addr:             *dnsAddr,
			InterceptIP:      ip,
			InterceptDomains: splitCSV(*intercept),
			Upstream:         splitCSV(*upstream),
			Logger:           logger,
		}
		go func() {
			logger.Info("dns listening", "addr", *dnsAddr, "intercept_ip", ip.String(), "domains", dnsSrv.InterceptDomains)
			if err := dnsSrv.ListenAndServe(); err != nil {
				logger.Error("dns server failed", "err", err)
				stop()
			}
		}()
	}

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if dnsSrv != nil {
		_ = dnsSrv.Shutdown()
	}
}

func splitCSV(s string) []string {
	return splitOn(",", s)
}

// splitOn handles a custom separator so station names containing commas
// ("Radio, Free Bucharest") survive intact when listed with a pipe.
func splitOn(sep, s string) []string {
	parts := strings.Split(s, sep)
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// autoBaseURL synthesizes a URL the AVR can actually reach when --base-url
// isn't set. Receivers need an IP they can connect to, not "localhost".
func autoBaseURL(httpAddr string) string {
	host, port, err := net.SplitHostPort(httpAddr)
	if err != nil {
		port = strings.TrimPrefix(httpAddr, ":")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = firstLocalIPv4()
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "80" || port == "" {
		return "http://" + host
	}
	return "http://" + host + ":" + port
}

func hostFromBaseURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func firstLocalIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		if v4 := ipn.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}
