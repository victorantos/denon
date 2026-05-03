// Package vtuner emits the XML protocol that vTuner-compatible AVRs (Denon,
// Marantz, Yamaha, Onkyo, Pioneer, etc.) speak. A response is a flat
// <ListOfItems> with polymorphic <Item> rows distinguished by <ItemType>.
package vtuner

import (
	"encoding/xml"
	"strconv"
)

// Page is the root XML response.
type Page struct {
	XMLName     xml.Name `xml:"ListOfItems"`
	ItemCount   int      `xml:"ItemCount"`
	NoDataCache string   `xml:"NoDataCache,omitempty"`
	Items       []Item   `xml:"Item"`
}

func NewPage(items ...Item) *Page {
	return &Page{ItemCount: len(items), Items: items}
}

// Uncached marks the page as non-cacheable — needed for menus whose contents
// change between requests, like search results.
func (p *Page) Uncached() *Page {
	p.NoDataCache = "Yes"
	return p
}

// Item carries every possible field for every variant. encoding/xml has no
// clean tagged-union support, so we lean on omitempty to elide unused fields.
type Item struct {
	ItemType string `xml:"ItemType"`

	Display string `xml:"Display,omitempty"`

	UrlPrevious       string `xml:"UrlPrevious,omitempty"`
	UrlPreviousBackUp string `xml:"UrlPreviousBackUp,omitempty"`

	Title        string `xml:"Title,omitempty"`
	UrlDir       string `xml:"UrlDir,omitempty"`
	UrlDirBackUp string `xml:"UrlDirBackUp,omitempty"`
	DirCount     int    `xml:"DirCount,omitempty"`

	SearchURL          string `xml:"SearchURL,omitempty"`
	SearchURLBackUp    string `xml:"SearchURLBackUp,omitempty"`
	SearchCaption      string `xml:"SearchCaption,omitempty"`
	SearchTextbox      string `xml:"SearchTextbox,omitempty"`
	SearchButtonGo     string `xml:"SearchButtonGo,omitempty"`
	SearchButtonCancel string `xml:"SearchButtonCancel,omitempty"`

	StationID        string `xml:"StationId,omitempty"`
	StationName      string `xml:"StationName,omitempty"`
	StationURL       string `xml:"StationUrl,omitempty"`
	StationDesc      string `xml:"StationDesc,omitempty"`
	Logo             string `xml:"Logo,omitempty"`
	StationFormat    string `xml:"StationFormat,omitempty"`
	StationLocation  string `xml:"StationLocation,omitempty"`
	StationBandWidth string `xml:"StationBandWidth,omitempty"`
	StationMime      string `xml:"StationMime,omitempty"`
	Relia            string `xml:"Relia,omitempty"`
	Bookmark         string `xml:"Bookmark,omitempty"`
}

func Display(text string) Item {
	return Item{ItemType: "Display", Display: text}
}

func Previous(url string) Item {
	return Item{ItemType: "Previous", UrlPrevious: url, UrlPreviousBackUp: url}
}

func Dir(title, url string, count int) Item {
	return Item{
		ItemType:     "Dir",
		Title:        title,
		UrlDir:       url,
		UrlDirBackUp: url,
		DirCount:     count,
	}
}

func Search(url, caption string) Item {
	return Item{
		ItemType:           "Search",
		SearchURL:          url,
		SearchURLBackUp:    url,
		SearchCaption:      caption,
		SearchButtonGo:     "Go",
		SearchButtonCancel: "Cancel",
	}
}

// Station is the playable-stream metadata. Bandwidth is kbps.
type Station struct {
	ID, Name, URL, Description, Logo string
	Format, Location, Mime           string
	Bandwidth                        int
}

func StationItem(s Station) Item {
	bw := ""
	if s.Bandwidth > 0 {
		bw = strconv.Itoa(s.Bandwidth)
	}
	return Item{
		ItemType:         "Station",
		StationID:        s.ID,
		StationName:      s.Name,
		StationURL:       s.URL,
		StationDesc:      s.Description,
		Logo:             s.Logo,
		StationFormat:    s.Format,
		StationLocation:  s.Location,
		StationBandWidth: bw,
		StationMime:      s.Mime,
		Relia:            "3", // 1-3 signal bars; receivers render this verbatim
	}
}
