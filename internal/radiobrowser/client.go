// Package radiobrowser is a minimal client for the community-run
// https://www.radio-browser.info JSON API. Mirrors rotate via DNS, so we
// resolve a friendly hostname periodically and use HTTPS against it.
package radiobrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	bootstrapHost = "all.api.radio-browser.info"
	mirrorTTL     = time.Hour
)

// Station is the subset of radio-browser station fields we use.
type Station struct {
	UUID        string `json:"stationuuid"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	URLResolved string `json:"url_resolved"`
	Homepage    string `json:"homepage"`
	Favicon     string `json:"favicon"`
	Tags        string `json:"tags"`
	Country     string `json:"country"`
	CountryCode string `json:"countrycode"`
	Language    string `json:"language"`
	Codec       string `json:"codec"`
	Bitrate     int    `json:"bitrate"`
	Votes       int    `json:"votes"`
	LastCheckOK int    `json:"lastcheckok"`
}

// NameCount is the {name, stationcount} shape used for tags, countries,
// languages. ISOCode is populated only for countries.
type NameCount struct {
	Name         string `json:"name"`
	StationCount int    `json:"stationcount"`
	ISOCode      string `json:"iso_3166_1,omitempty"`
}

// Client is safe for concurrent use.
type Client struct {
	UserAgent string
	HTTP      *http.Client

	mu      sync.Mutex
	base    string
	expires time.Time
}

func NewClient(userAgent string) *Client {
	return &Client{
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Base returns a usable mirror URL like "https://de1.api.radio-browser.info".
// Mirrors come and go, so we re-pick periodically.
func (c *Client) Base(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base != "" && time.Now().Before(c.expires) {
		return c.base, nil
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, bootstrapHost)
	if err != nil {
		return "", fmt.Errorf("mirror discovery: %w", err)
	}
	if len(ips) == 0 {
		return "", errors.New("mirror discovery returned no addresses")
	}
	pick := ips[rand.IntN(len(ips))]
	host := pick
	// Reverse-lookup recovers the friendly hostname (e.g. de1.api...). We need
	// a name, not an IP, for HTTPS — TLS validation is keyed on hostname.
	if names, err := net.DefaultResolver.LookupAddr(ctx, pick); err == nil && len(names) > 0 {
		host = strings.TrimSuffix(names[0], ".")
	}
	c.base = "https://" + host
	c.expires = time.Now().Add(mirrorTTL)
	return c.base, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	base, err := c.Base(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("radio-browser %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func popularQuery(extra ...string) string {
	q := url.Values{}
	q.Set("hidebroken", "true")
	q.Set("order", "stationcount")
	q.Set("reverse", "true")
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return q.Encode()
}

func (c *Client) Tags(ctx context.Context, limit int) ([]NameCount, error) {
	var out []NameCount
	return out, c.get(ctx, "/json/tags?"+popularQuery("limit", fmt.Sprint(limit)), &out)
}

func (c *Client) Countries(ctx context.Context) ([]NameCount, error) {
	var out []NameCount
	return out, c.get(ctx, "/json/countries?"+popularQuery(), &out)
}

func (c *Client) Languages(ctx context.Context) ([]NameCount, error) {
	var out []NameCount
	return out, c.get(ctx, "/json/languages?"+popularQuery(), &out)
}

func stationQuery(limit int) string {
	if limit <= 0 {
		limit = 100
	}
	q := url.Values{}
	q.Set("hidebroken", "true")
	q.Set("order", "clickcount")
	q.Set("reverse", "true")
	q.Set("limit", fmt.Sprint(limit))
	return q.Encode()
}

func (c *Client) TopVote(ctx context.Context, limit int) ([]Station, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []Station
	return out, c.get(ctx, fmt.Sprintf("/json/stations/topvote/%d?hidebroken=true", limit), &out)
}

func (c *Client) ByTag(ctx context.Context, tag string, limit int) ([]Station, error) {
	var out []Station
	return out, c.get(ctx, "/json/stations/bytag/"+url.PathEscape(tag)+"?"+stationQuery(limit), &out)
}

func (c *Client) ByCountryCode(ctx context.Context, code string, limit int) ([]Station, error) {
	var out []Station
	return out, c.get(ctx, "/json/stations/bycountrycodeexact/"+url.PathEscape(strings.ToUpper(code))+"?"+stationQuery(limit), &out)
}

func (c *Client) ByLanguage(ctx context.Context, lang string, limit int) ([]Station, error) {
	var out []Station
	return out, c.get(ctx, "/json/stations/bylanguageexact/"+url.PathEscape(lang)+"?"+stationQuery(limit), &out)
}

func (c *Client) Search(ctx context.Context, name string, limit int) ([]Station, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []Station
	q := url.Values{}
	q.Set("name", name)
	q.Set("hidebroken", "true")
	q.Set("order", "clickcount")
	q.Set("reverse", "true")
	q.Set("limit", fmt.Sprint(limit))
	return out, c.get(ctx, "/json/stations/search?"+q.Encode(), &out)
}

// ResolveAndCount returns the playable stream URL for a station UUID and
// reports the click to the community DB in the same round-trip.
func (c *Client) ResolveAndCount(ctx context.Context, uuid string) (string, error) {
	var r struct {
		OK   bool   `json:"ok"`
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := c.get(ctx, "/json/url/"+url.PathEscape(uuid), &r); err != nil {
		return "", err
	}
	if r.URL == "" {
		return "", errors.New("no url resolved")
	}
	return r.URL, nil
}
