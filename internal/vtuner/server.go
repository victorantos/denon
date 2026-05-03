package vtuner

import (
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

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

// handleSetupApp catches the hardcoded vTuner endpoint the receiver hits
// first, e.g. /setupapp/Denon/asp/BrowseXML/loginXML.asp. Path fragments
// differ per brand; the response shape is identical so we funnel into root.
func (s *Server) handleSetupApp(w http.ResponseWriter, r *http.Request) {
	s.handleRoot(w, r)
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
	http.Redirect(w, r, stream, http.StatusFound)
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
