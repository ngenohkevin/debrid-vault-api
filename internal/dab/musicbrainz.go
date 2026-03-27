package dab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const mbBaseURL = "https://musicbrainz.org/ws/2"
const mbUserAgent = "DebridVault/1.0.0 (https://github.com/ngenohkevin/debrid-vault-api)"

// MBClient queries MusicBrainz for metadata enrichment.
type MBClient struct {
	http    *http.Client
	limiter *rate.Limiter
	// Cache release data per "artist|album" to avoid redundant lookups
	releaseCache map[string]*MBRelease
	cacheMu      sync.RWMutex
}

// MBRecording holds enriched recording data from MusicBrainz.
type MBRecording struct {
	TrackMBID       string
	ArtistMBID      string
	AlbumMBID       string
	AlbumArtistMBID string
	ReleaseGroupID  string
	ISRC            string
	Label           string
	CatalogNumber   string
	Barcode         string
	Genres          []string
}

// MBRelease holds cached release-level data.
type MBRelease struct {
	ID             string
	ArtistMBID     string
	ReleaseGroupID string
	Label          string
	CatalogNumber  string
	Barcode        string
	Genres         []string
}

func NewMBClient() *MBClient {
	return &MBClient{
		http:         &http.Client{Timeout: 15 * time.Second},
		limiter:      rate.NewLimiter(rate.Every(time.Second), 1),
		releaseCache: make(map[string]*MBRelease),
	}
}

// EnrichTrack looks up a track on MusicBrainz and returns enriched metadata.
func (c *MBClient) EnrichTrack(title, artist, album, isrc string) *MBRecording {
	var rec *MBRecording

	// Try ISRC lookup first (most reliable)
	if isrc != "" {
		rec = c.lookupByISRC(isrc)
	}

	// Fall back to search
	if rec == nil {
		rec = c.searchRecording(title, artist, album)
	}

	if rec == nil {
		return nil
	}

	// Enrich with cached release data
	cacheKey := strings.ToLower(artist + "|" + album)
	c.cacheMu.RLock()
	cached := c.releaseCache[cacheKey]
	c.cacheMu.RUnlock()

	if cached != nil {
		rec.AlbumMBID = cached.ID
		rec.AlbumArtistMBID = cached.ArtistMBID
		rec.ReleaseGroupID = cached.ReleaseGroupID
		rec.Label = cached.Label
		rec.CatalogNumber = cached.CatalogNumber
		rec.Barcode = cached.Barcode
		if len(rec.Genres) == 0 {
			rec.Genres = cached.Genres
		}
	}

	return rec
}

func (c *MBClient) lookupByISRC(isrc string) *MBRecording {
	data, err := c.get(fmt.Sprintf("/isrc/%s?fmt=json&inc=artists+releases+tags+genres", isrc))
	if err != nil {
		return nil
	}

	var resp struct {
		Recordings []mbRecordingJSON `json:"recordings"`
	}
	if json.Unmarshal(data, &resp) != nil || len(resp.Recordings) == 0 {
		return nil
	}

	return c.parseRecording(&resp.Recordings[0])
}

func (c *MBClient) searchRecording(title, artist, album string) *MBRecording {
	// Strict search first
	query := fmt.Sprintf(`recording:"%s" AND artist:"%s"`, escapeQuery(title), escapeQuery(artist))
	if album != "" {
		query += fmt.Sprintf(` AND release:"%s"`, escapeQuery(album))
	}

	rec := c.doSearch(query, 1)
	if rec != nil {
		return rec
	}

	// Loose search fallback
	query = fmt.Sprintf(`recording:(%s) AND artist:(%s)`, escapeQuery(title), escapeQuery(artist))
	return c.doSearch(query, 3)
}

func (c *MBClient) doSearch(query string, limit int) *MBRecording {
	path := fmt.Sprintf("/recording?query=%s&fmt=json&limit=%d", url.QueryEscape(query), limit)
	data, err := c.get(path)
	if err != nil {
		return nil
	}

	var resp struct {
		Recordings []mbRecordingJSON `json:"recordings"`
	}
	if json.Unmarshal(data, &resp) != nil || len(resp.Recordings) == 0 {
		return nil
	}

	// Use highest score result
	return c.parseRecording(&resp.Recordings[0])
}

func (c *MBClient) parseRecording(r *mbRecordingJSON) *MBRecording {
	rec := &MBRecording{
		TrackMBID: r.ID,
	}

	// Artist MBID
	if len(r.ArtistCredit) > 0 {
		rec.ArtistMBID = r.ArtistCredit[0].Artist.ID
	}

	// ISRCs
	if len(r.ISRCs) > 0 {
		rec.ISRC = r.ISRCs[0]
	}

	// Genres/tags
	for _, g := range r.Genres {
		rec.Genres = append(rec.Genres, g.Name)
	}
	if len(rec.Genres) == 0 {
		for _, t := range r.Tags {
			if t.Count >= 1 {
				rec.Genres = append(rec.Genres, t.Name)
			}
		}
	}

	// Release data (cache it)
	if len(r.Releases) > 0 {
		rel := &r.Releases[0]
		release := &MBRelease{
			ID: rel.ID,
		}
		if len(rel.ArtistCredit) > 0 {
			release.ArtistMBID = rel.ArtistCredit[0].Artist.ID
		}
		if rel.ReleaseGroup != nil {
			release.ReleaseGroupID = rel.ReleaseGroup.ID
		}
		if len(rel.LabelInfo) > 0 {
			if rel.LabelInfo[0].Label != nil {
				release.Label = rel.LabelInfo[0].Label.Name
			}
			release.CatalogNumber = rel.LabelInfo[0].CatalogNumber
		}
		release.Barcode = rel.Barcode
		release.Genres = rec.Genres

		rec.AlbumMBID = release.ID
		rec.AlbumArtistMBID = release.ArtistMBID
		rec.ReleaseGroupID = release.ReleaseGroupID
		rec.Label = release.Label
		rec.CatalogNumber = release.CatalogNumber
		rec.Barcode = release.Barcode

		// Cache for other tracks from same album
		if len(r.ArtistCredit) > 0 && len(r.Releases) > 0 {
			cacheKey := strings.ToLower(r.ArtistCredit[0].Artist.Name + "|" + rel.Title)
			c.cacheMu.Lock()
			c.releaseCache[cacheKey] = release
			c.cacheMu.Unlock()
		}
	}

	return rec
}

func (c *MBClient) get(path string) ([]byte, error) {
	// Rate limit: 1 request per second
	c.limiter.Wait(context.Background())

	req, err := http.NewRequest("GET", mbBaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", mbUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 503 {
		log.Println("MusicBrainz: rate limited, skipping")
		return nil, fmt.Errorf("rate limited")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("MusicBrainz %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func escapeQuery(s string) string {
	// Escape Lucene special chars
	replacer := strings.NewReplacer(
		`"`, `\"`,
		`\`, `\\`,
		`+`, `\+`,
		`-`, `\-`,
		`(`, `\(`,
		`)`, `\)`,
		`[`, `\[`,
		`]`, `\]`,
		`{`, `\{`,
		`}`, `\}`,
		`^`, `\^`,
		`~`, `\~`,
		`*`, `\*`,
		`?`, `\?`,
		`:`, `\:`,
		`/`, `\/`,
	)
	return replacer.Replace(s)
}

// JSON types for MusicBrainz API responses

type mbRecordingJSON struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Length       int              `json:"length"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
	Releases     []mbReleaseJSON  `json:"releases"`
	ISRCs        []string         `json:"isrcs"`
	Genres       []mbTag          `json:"genres"`
	Tags         []mbTag          `json:"tags"`
}

type mbArtistCredit struct {
	Artist struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
}

type mbReleaseJSON struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Date         string           `json:"date"`
	Country      string           `json:"country"`
	Barcode      string           `json:"barcode"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
	LabelInfo    []mbLabelInfo    `json:"label-info"`
	ReleaseGroup *mbReleaseGroup  `json:"release-group"`
}

type mbLabelInfo struct {
	CatalogNumber string `json:"catalog-number"`
	Label         *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"label"`
}

type mbReleaseGroup struct {
	ID string `json:"id"`
}

type mbTag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
