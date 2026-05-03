package vtuner

import (
	"context"
	"encoding/xml"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"denon/internal/radiobrowser"
)

// xmlPreamble matches what YCast and YTuner emit; some firmwares are picky
// about the exact preamble, so we mirror the proven-working form.
const xmlPreamble = `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>` + "\n"

// Server handles vTuner protocol requests and renders Pages back to receivers.
type Server struct {
	BaseURL string // public URL receivers reach us at, e.g. "http://192.168.1.10"
	Logger  *slog.Logger
	Radio   *radiobrowser.Client

	// LegacyStations is an explicit, ordered list of station names to use as
	// the fallback pool. Each name is looked up via radio-browser search
	// (top result wins). When set, this overrides LegacyCountries entirely —
	// you get exactly the stations you asked for, hashed across favorites.
	LegacyStations []string

	// LegacyCountries narrows the fallback pool to specific country codes
	// (ISO 3166-1 alpha-2). Empty = global top-voted. Ignored if
	// LegacyStations is set.
	LegacyCountries []string

	// LegacyExcludeTerms drops any station whose tags or name (case-insensitive
	// substring match) hits one of these. Useful when a country's "popular"
	// list contains a genre you actively don't want. Applies to country/global
	// pools; ignored when LegacyStations is set (you asked for those by name).
	LegacyExcludeTerms []string

	// Cache of HTTP-only popular stations used as legacy-favorite fallbacks.
	// Each unique cached vTuner ID gets stably hashed into this list so the
	// same Favorite always plays the same station, but different Favorites
	// play different stations.
	legacyMu       sync.Mutex
	legacyStations []radiobrowser.Station
	legacyExpires  time.Time
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/setupapp/", s.handleSetupApp)

	mux.HandleFunc("/radiobrowser", s.handleRBRoot)
	mux.HandleFunc("/radiobrowser/popular", s.handleRBPopular)
	mux.HandleFunc("/radiobrowser/genres", s.handleRBGenres)
	mux.HandleFunc("/radiobrowser/genre/{tag}", s.handleRBByTag)
	mux.HandleFunc("/radiobrowser/countries", s.handleRBCountries)
	mux.HandleFunc("/radiobrowser/country/{code}", s.handleRBByCountry)
	mux.HandleFunc("/radiobrowser/languages", s.handleRBLanguages)
	mux.HandleFunc("/radiobrowser/language/{lang}", s.handleRBByLanguage)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/play", s.handlePlay)

	return s.logging(mux)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"remote", r.RemoteAddr,
			"ua", r.UserAgent())
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	page := NewPage(
		Display("denon — pick a source"),
		Dir("Radio Browser", s.makeURL("/radiobrowser"), 4),
		Search(s.makeURL("/search"), "Search stations"),
	)
	writeXML(w, page)
}

// handleSetupApp catches every /setupapp/* request the receiver makes. The
// receiver uses two distinct subpaths: BrowseXML/* for directory navigation,
// and asp/func/dynamOD.asp for legacy-favorites playback. We dispatch
// accordingly.
func (s *Server) handleSetupApp(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/asp/func/dynamOD.asp") {
		s.handleLegacyPlay(w, r)
		return
	}
	s.handleRoot(w, r)
}

// handleLegacyPlay answers /setupapp/<vendor>/asp/func/dynamOD.asp. These
// requests carry pre-shutdown numeric vTuner station IDs from the receiver's
// cached Favorites list. We don't have the original mapping from those IDs
// to current stations, so we hash each ID into a list of popular HTTP-only
// stations and play that one — different Favorites get different stations,
// and the same Favorite plays the same station consistently across taps.
func (s *Server) handleLegacyPlay(w http.ResponseWriter, r *http.Request) {
	stations, err := s.legacyFallbacks(r.Context())
	if err != nil || len(stations) == 0 {
		http.Error(w, "no fallback stations available", http.StatusBadGateway)
		return
	}
	id := r.URL.Query().Get("id")
	idx := stableIdx(id, len(stations))
	chosen := stations[idx]
	streamURL := chosen.URLResolved
	if streamURL == "" {
		streamURL = chosen.URL
	}
	s.Logger.Info("legacy play", "id", id, "idx", idx, "station", chosen.Name, "stream", streamURL)
	s.proxyStream(w, r, streamURL)
}

// legacyFallbacks returns a cached list of HTTP-only stations to back legacy
// favorites. By default it pulls global top-voted; if LegacyCountries is set,
// it pulls per-country popular stations and dedups by UUID. The proxy will
// happily handle HTTPS upstreams too, but HTTP-only sources are a tiny win
// (one fewer TLS handshake per station tap) so we filter when we can.
// Cached for an hour because radio-browser mirror discovery isn't free.
func (s *Server) legacyFallbacks(ctx context.Context) ([]radiobrowser.Station, error) {
	s.legacyMu.Lock()
	defer s.legacyMu.Unlock()
	if len(s.legacyStations) > 0 && time.Now().Before(s.legacyExpires) {
		return s.legacyStations, nil
	}

	var stations []radiobrowser.Station

	if len(s.LegacyStations) > 0 {
		seen := map[string]bool{}
		for _, name := range s.LegacyStations {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			results, err := s.Radio.Search(ctx, name, 10)
			if err != nil {
				s.Logger.Warn("legacy station search failed", "name", name, "err", err)
				continue
			}
			if len(results) == 0 {
				s.Logger.Warn("legacy station not found in radio-browser", "name", name)
				continue
			}
			chosen, ok := pickBestMatch(name, results)
			if !ok {
				s.Logger.Warn("legacy station has only HLS streams (X3000-class receivers can't decode HLS)", "name", name)
				continue
			}
			if seen[chosen.UUID] {
				continue
			}
			seen[chosen.UUID] = true
			stations = append(stations, chosen)
		}
		if len(stations) > 0 {
			s.legacyStations = stations
			s.legacyExpires = time.Now().Add(time.Hour)
			names := make([]string, 0, len(stations))
			for _, st := range stations {
				names = append(names, st.Name)
			}
			s.Logger.Info("legacy fallback pool refreshed (explicit list)", "count", len(stations), "stations", names)
			return stations, nil
		}
		s.Logger.Warn("legacy stations list yielded nothing — falling through to country/global")
	}

	if len(s.LegacyCountries) > 0 {
		seen := map[string]bool{}
		for _, cc := range s.LegacyCountries {
			cc = strings.TrimSpace(cc)
			if cc == "" {
				continue
			}
			byCC, err := s.Radio.ByCountryCode(ctx, cc, 100)
			if err != nil {
				s.Logger.Warn("legacy country fetch failed", "cc", cc, "err", err)
				continue
			}
			for _, st := range byCC {
				if seen[st.UUID] {
					continue
				}
				seen[st.UUID] = true
				stations = append(stations, st)
			}
		}
		// If every per-country call failed, fall through to global top so the
		// receiver still hears music instead of bad-gateway errors.
		if len(stations) == 0 {
			global, err := s.Radio.TopVote(ctx, 200)
			if err != nil {
				return nil, err
			}
			stations = global
		}
	} else {
		global, err := s.Radio.TopVote(ctx, 200)
		if err != nil {
			return nil, err
		}
		stations = global
	}

	httpOnly := make([]radiobrowser.Station, 0, len(stations))
	for _, st := range stations {
		if matchesExcluded(st, s.LegacyExcludeTerms) {
			continue
		}
		u := st.URLResolved
		if u == "" {
			u = st.URL
		}
		if strings.HasPrefix(u, "http://") {
			httpOnly = append(httpOnly, st)
		}
	}
	if len(httpOnly) == 0 {
		// Don't fall through to "all stations" if exclusion zeroed us out;
		// repeat the exclude pass on the full set so the user's filter holds.
		for _, st := range stations {
			if matchesExcluded(st, s.LegacyExcludeTerms) {
				continue
			}
			httpOnly = append(httpOnly, st)
		}
	}
	s.legacyStations = httpOnly
	s.legacyExpires = time.Now().Add(time.Hour)
	s.Logger.Info("legacy fallback pool refreshed",
		"count", len(s.legacyStations),
		"countries", s.LegacyCountries,
		"excluded_terms", s.LegacyExcludeTerms)
	return httpOnly, nil
}

// pickBestMatch picks the best radio-browser hit for a user-supplied station
// name. Exact-name matches beat prefix matches beat substring matches.
// Within a tier, non-HLS URLs are strongly preferred (a large score penalty
// for HLS) because older AVRs can't decode HLS at all. Returns ok=false if
// every candidate is HLS-only.
func pickBestMatch(query string, results []radiobrowser.Station) (radiobrowser.Station, bool) {
	qLower := strings.ToLower(strings.TrimSpace(query))
	var best radiobrowser.Station
	bestScore := -1
	for _, st := range results {
		nameLower := strings.ToLower(st.Name)
		score := 0
		switch {
		case nameLower == qLower:
			score = 1000
		case strings.HasPrefix(nameLower, qLower):
			score = 500
		case strings.Contains(nameLower, qLower):
			score = 100
		}
		u := st.URLResolved
		if u == "" {
			u = st.URL
		}
		if strings.Contains(strings.ToLower(u), ".m3u8") {
			score -= 10000
		}
		if score > bestScore {
			bestScore = score
			best = st
		}
	}
	if bestScore < 0 {
		return radiobrowser.Station{}, false
	}
	return best, true
}

func matchesExcluded(st radiobrowser.Station, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	haystack := strings.ToLower(st.Tags + " " + st.Name)
	for _, t := range terms {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			continue
		}
		if strings.Contains(haystack, t) {
			return true
		}
	}
	return false
}

// stableIdx maps an arbitrary string to [0,n) deterministically — same input
// always yields the same index.
func stableIdx(s string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum32() % uint32(n))
}

func (s *Server) handleRBRoot(w http.ResponseWriter, r *http.Request) {
	page := NewPage(
		Display("Radio Browser"),
		Previous(s.makeURL("/")),
		Dir("Most Popular", s.makeURL("/radiobrowser/popular"), 100),
		Dir("Genres", s.makeURL("/radiobrowser/genres"), 0),
		Dir("Countries", s.makeURL("/radiobrowser/countries"), 0),
		Dir("Languages", s.makeURL("/radiobrowser/languages"), 0),
	)
	writeXML(w, page)
}

func (s *Server) handleRBPopular(w http.ResponseWriter, r *http.Request) {
	stations, err := s.Radio.TopVote(r.Context(), 100)
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	s.writeStations(w, "Most Popular", "/radiobrowser", stations)
}

func (s *Server) handleRBGenres(w http.ResponseWriter, r *http.Request) {
	tags, err := s.Radio.Tags(r.Context(), 200)
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	items := []Item{Display("Genres"), Previous(s.makeURL("/radiobrowser"))}
	for _, t := range tags {
		if t.Name == "" {
			continue
		}
		items = append(items, Dir(t.Name, s.makeURL("/radiobrowser/genre/"+t.Name), t.StationCount))
	}
	writeXML(w, NewPage(items...))
}

func (s *Server) handleRBByTag(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	stations, err := s.Radio.ByTag(r.Context(), tag, 100)
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	s.writeStations(w, tag, "/radiobrowser/genres", stations)
}

func (s *Server) handleRBCountries(w http.ResponseWriter, r *http.Request) {
	countries, err := s.Radio.Countries(r.Context())
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	items := []Item{Display("Countries"), Previous(s.makeURL("/radiobrowser"))}
	for _, c := range countries {
		if c.ISOCode == "" {
			continue
		}
		items = append(items, Dir(c.Name, s.makeURL("/radiobrowser/country/"+c.ISOCode), c.StationCount))
	}
	writeXML(w, NewPage(items...))
}

func (s *Server) handleRBByCountry(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	stations, err := s.Radio.ByCountryCode(r.Context(), code, 100)
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	s.writeStations(w, code, "/radiobrowser/countries", stations)
}

func (s *Server) handleRBLanguages(w http.ResponseWriter, r *http.Request) {
	langs, err := s.Radio.Languages(r.Context())
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	items := []Item{Display("Languages"), Previous(s.makeURL("/radiobrowser"))}
	for _, l := range langs {
		if l.Name == "" {
			continue
		}
		items = append(items, Dir(l.Name, s.makeURL("/radiobrowser/language/"+l.Name), l.StationCount))
	}
	writeXML(w, NewPage(items...))
}

func (s *Server) handleRBByLanguage(w http.ResponseWriter, r *http.Request) {
	lang := r.PathValue("lang")
	stations, err := s.Radio.ByLanguage(r.Context(), lang, 100)
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	s.writeStations(w, lang, "/radiobrowser/languages", stations)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("search"))
	if q == "" {
		// First call — show the search box. AVRs POST back the textbox value
		// to the same URL, so we just render Search again for empty input.
		writeXML(w, NewPage(
			Display("Search"),
			Previous(s.makeURL("/")),
			Search(s.makeURL("/search"), "Search station names"),
		).Uncached())
		return
	}
	stations, err := s.Radio.Search(r.Context(), q, 100)
	if err != nil {
		s.errorPage(w, "Radio Browser unavailable", err)
		return
	}
	s.writeStations(w, "Results: "+q, "/", stations)
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("id")
	if uuid == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	stream, err := s.Radio.ResolveAndCount(r.Context(), uuid)
	if err != nil {
		s.Logger.Error("resolve stream url", "uuid", uuid, "err", err)
		http.Error(w, "stream unavailable", http.StatusBadGateway)
		return
	}
	s.proxyStream(w, r, stream)
}

// proxyStream pipes audio bytes from the upstream URL back to the receiver
// over plain HTTP. Older AVRs (e.g. AVR-X3000) cannot follow HTTPS
// redirects or do TLS at all on the playback path, so we terminate TLS
// here and hand them clean HTTP. ICY metadata is forwarded so receivers
// that show track titles still get them.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	upReq, err := http.NewRequestWithContext(r.Context(), "GET", upstreamURL, nil)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusInternalServerError)
		return
	}
	if v := r.Header.Get("Icy-MetaData"); v != "" {
		upReq.Header.Set("Icy-MetaData", v)
	}
	upReq.Header.Set("User-Agent", "denon/0.1 (proxy)")

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		s.Logger.Error("proxy upstream", "url", upstreamURL, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "icy-") {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) writeStations(w http.ResponseWriter, title, backPath string, stations []radiobrowser.Station) {
	items := []Item{Display(title), Previous(s.makeURL(backPath))}
	for _, st := range stations {
		items = append(items, StationItem(toVTunerStation(s.makeURL("/play"), st)))
	}
	writeXML(w, NewPage(items...))
}

func (s *Server) errorPage(w http.ResponseWriter, title string, err error) {
	s.Logger.Error("upstream error", "title", title, "err", err)
	writeXML(w, NewPage(
		Display(title),
		Display(err.Error()),
		Previous(s.makeURL("/")),
	).Uncached())
}

// makeURL builds an absolute URL the receiver will follow next. The seed
// vtuner=true param exists because some firmwares append extra params with '&'
// regardless of whether the URL already has a query — without a seed, the
// resulting URL is malformed.
func (s *Server) makeURL(path string) string {
	u, _ := url.Parse(s.BaseURL)
	u.Path = path
	q := u.Query()
	q.Set("vtuner", "true")
	u.RawQuery = q.Encode()
	return u.String()
}

func toVTunerStation(playBaseURL string, s radiobrowser.Station) Station {
	playURL := playBaseURL + "&id=" + url.QueryEscape(s.UUID)
	desc := s.Tags
	if s.Country != "" {
		if desc != "" {
			desc += " · "
		}
		desc += s.Country
	}
	return Station{
		ID:          s.UUID,
		Name:        s.Name,
		URL:         playURL,
		Description: desc,
		Logo:        s.Favicon,
		Format:      firstTag(s.Tags),
		Location:    s.CountryCode,
		Mime:        codecToMime(s.Codec),
		Bandwidth:   s.Bitrate,
	}
}

func firstTag(tags string) string {
	if i := strings.IndexByte(tags, ','); i > 0 {
		return strings.TrimSpace(tags[:i])
	}
	return strings.TrimSpace(tags)
}

// codecToMime maps the codec strings radio-browser uses to the MIME types
// receivers display under the station name. Unknown codecs pass through.
func codecToMime(codec string) string {
	switch strings.ToUpper(codec) {
	case "MP3":
		return "MP3"
	case "AAC", "AAC+", "AACP":
		return "AAC"
	case "OGG":
		return "OGG"
	case "FLAC":
		return "FLAC"
	default:
		return codec
	}
}

func writeXML(w http.ResponseWriter, page *Page) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = w.Write([]byte(xmlPreamble))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(page)
	_ = enc.Flush()
}
