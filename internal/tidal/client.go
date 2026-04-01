package tidal

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/ngenohkevin/debrid-vault-api/internal/dab"
)

// Client communicates with the hifi-api-workers instance.
type Client struct {
	http    *http.Client
	baseURL string // e.g. http://localhost:8787
}

// NewClient creates a new Tidal API client pointing at the hifi-api-workers.
func NewClient(baseURL string) *Client {
	return &Client{
		http:    &http.Client{Timeout: 60 * time.Second},
		baseURL: baseURL,
	}
}

func (c *Client) get(path string, params url.Values) (json.RawMessage, error) {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tidal API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tidal API read failed: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tidal API error %d: %s", resp.StatusCode, string(body))
	}

	// The hifi-api-workers wraps responses in {"version":"2.7","data":...}
	var wrapper tidalResponse
	if err := json.Unmarshal(body, &wrapper); err != nil {
		// Not wrapped — return raw
		return body, nil
	}
	if wrapper.Data != nil {
		return wrapper.Data, nil
	}
	return body, nil
}

// Search searches for tracks, albums, or artists.
func (c *Client) Search(query, searchType string, limit int) (*dab.SearchResult, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}

	// Map search type to the hifi-api query parameter
	switch searchType {
	case "album":
		params.Set("al", query)
	case "artist":
		params.Set("a", query)
	default: // "track" or empty
		params.Set("s", query)
	}

	data, err := c.get("/search", params)
	if err != nil {
		return nil, err
	}

	result := &dab.SearchResult{}

	// Try top-hits style response (album/artist search returns nested tracks/albums/artists)
	var topHits tidalSearchResult
	if err := json.Unmarshal(data, &topHits); err == nil {
		if topHits.Tracks != nil {
			var tracks []tidalTrack
			json.Unmarshal(topHits.Tracks.Items, &tracks)
			for _, t := range tracks {
				result.Tracks = append(result.Tracks, t.toDABTrack())
			}
		}
		if topHits.Albums != nil {
			var albums []tidalAlbum
			json.Unmarshal(topHits.Albums.Items, &albums)
			for _, a := range albums {
				result.Albums = append(result.Albums, a.toDABAlbum())
			}
		}
		if topHits.Artists != nil {
			var artists []tidalArtist
			json.Unmarshal(topHits.Artists.Items, &artists)
			for _, a := range artists {
				result.Artists = append(result.Artists, dab.Artist{
					ID:      json.Number(fmt.Sprintf("%d", a.ID)),
					Name:    a.Name,
					Picture: artistPictureURL(a.Picture),
				})
			}
		}
		// Only return if we actually got results from the nested format
		if len(result.Tracks) > 0 || len(result.Albums) > 0 || len(result.Artists) > 0 {
			return result, nil
		}
	}

	// Flat paged list: track search returns {items:[...]} directly
	var pagedList struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &pagedList); err == nil && pagedList.Items != nil {
		switch searchType {
		case "album":
			var albums []tidalAlbum
			json.Unmarshal(pagedList.Items, &albums)
			for _, a := range albums {
				result.Albums = append(result.Albums, a.toDABAlbum())
			}
		case "artist":
			var artists []tidalArtist
			json.Unmarshal(pagedList.Items, &artists)
			for _, a := range artists {
				result.Artists = append(result.Artists, dab.Artist{
					ID:      json.Number(fmt.Sprintf("%d", a.ID)),
					Name:    a.Name,
					Picture: artistPictureURL(a.Picture),
				})
			}
		default:
			var tracks []tidalTrack
			json.Unmarshal(pagedList.Items, &tracks)
			for _, t := range tracks {
				result.Tracks = append(result.Tracks, t.toDABTrack())
			}
		}
	}

	return result, nil
}

// GetAlbum fetches album details with track list.
func (c *Client) GetAlbum(albumID string) (*dab.Album, error) {
	data, err := c.get("/album", url.Values{"id": {albumID}})
	if err != nil {
		return nil, err
	}

	// The album endpoint returns the album + items (tracks wrapped in {item: ...})
	var raw struct {
		tidalAlbum
		Items []struct {
			Item tidalTrack `json:"item"`
		} `json:"items"`
		FlatItems []tidalTrack `json:"-"` // fallback
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse album: %w", err)
	}

	// If items have nested "item" wrapper, extract; otherwise try flat
	var tracks []tidalTrack
	for _, wrapper := range raw.Items {
		if wrapper.Item.ID > 0 {
			tracks = append(tracks, wrapper.Item)
		}
	}
	// Fallback: try parsing items as flat tracks
	if len(tracks) == 0 {
		var flatRaw struct {
			Items []tidalTrack `json:"items"`
		}
		json.Unmarshal(data, &flatRaw)
		tracks = flatRaw.Items
	}

	album := raw.tidalAlbum.toDABAlbum()
	for _, t := range tracks {
		track := t.toDABTrack()
		// Fill in album-level data the track may not have
		if track.AlbumTitle == "" {
			track.AlbumTitle = album.Title
		}
		if track.AlbumCover == "" {
			track.AlbumCover = album.Cover
		}
		album.Tracks = append(album.Tracks, track)
	}

	// If no items, try fetching tracks separately
	if len(album.Tracks) == 0 {
		log.Printf("Album %s has no inline tracks, trying separate fetch", albumID)
	}

	return &album, nil
}

// GetArtist fetches artist info and their albums.
func (c *Client) GetArtist(artistID string) (*dab.DiscographyResult, error) {
	data, err := c.get("/artist", url.Values{"id": {artistID}})
	if err != nil {
		return nil, err
	}

	var raw struct {
		tidalArtist
		Albums []tidalAlbum `json:"albums"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse artist: %w", err)
	}

	result := &dab.DiscographyResult{
		Artist: dab.Artist{
			ID:      json.Number(fmt.Sprintf("%d", raw.ID)),
			Name:    raw.Name,
			Picture: artistPictureURL(raw.Picture),
		},
	}
	for _, a := range raw.Albums {
		result.Albums = append(result.Albums, a.toDABAlbum())
	}

	return result, nil
}

// GetTrackInfo fetches detailed track info (ISRC, track number, etc.)
func (c *Client) GetTrackInfo(trackID string) (*tidalTrack, error) {
	data, err := c.get("/info", url.Values{"id": {trackID}})
	if err != nil {
		return nil, err
	}

	var track tidalTrack
	if err := json.Unmarshal(data, &track); err != nil {
		return nil, fmt.Errorf("parse track info: %w", err)
	}
	return &track, nil
}

// GetPlaybackInfo gets stream URL or DASH manifest for a track.
func (c *Client) GetPlaybackInfo(trackID, quality string) (*tidalPlaybackInfo, error) {
	data, err := c.get("/track", url.Values{"id": {trackID}, "quality": {quality}})
	if err != nil {
		return nil, err
	}

	var info tidalPlaybackInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parse playback info: %w", err)
	}
	return &info, nil
}

// GetLyrics fetches lyrics for a track. Returns nil if no lyrics available.
func (c *Client) GetLyrics(trackID string) (*dab.Lyrics, error) {
	data, err := c.get("/lyrics", url.Values{"id": {trackID}})
	if err != nil {
		return nil, err
	}

	var raw struct {
		Lyrics *tidalLyricsResponse `json:"lyrics"`
	}
	// Try nested format first
	if err := json.Unmarshal(data, &raw); err == nil && raw.Lyrics != nil {
		return &dab.Lyrics{
			Lyrics:       raw.Lyrics.Lyrics,
			SyncedLyrics: raw.Lyrics.Subtitles,
		}, nil
	}

	// Try flat format
	var flat tidalLyricsResponse
	if err := json.Unmarshal(data, &flat); err == nil && flat.Lyrics != "" {
		return &dab.Lyrics{
			Lyrics:       flat.Lyrics,
			SyncedLyrics: flat.Subtitles,
		}, nil
	}

	return nil, nil
}

// CoverURL returns the URL for album cover art.
func CoverURL(coverID string) string {
	return coverURL(coverID)
}
