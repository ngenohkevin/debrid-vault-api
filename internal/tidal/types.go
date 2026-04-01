package tidal

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ngenohkevin/debrid-vault-api/internal/dab"
)

// Quality tiers ordered by preference (best first).
const (
	QualityHiResLossless = "HI_RES_LOSSLESS"
	QualityLossless      = "LOSSLESS"
	QualityHigh          = "HIGH"
)

// --- Raw Tidal API response types ---

type tidalResponse struct {
	Version string          `json:"version"`
	Data    json.RawMessage `json:"data"`
}

type tidalSearchResult struct {
	Tracks  *tidalPagedList `json:"tracks,omitempty"`
	Albums  *tidalPagedList `json:"albums,omitempty"`
	Artists *tidalPagedList `json:"artists,omitempty"`
}

type tidalPagedList struct {
	Items            json.RawMessage `json:"items"`
	TotalNumberItems int             `json:"totalNumberOfItems"`
	Limit            int             `json:"limit"`
	Offset           int             `json:"offset"`
}

type tidalTrack struct {
	ID              int              `json:"id"`
	Title           string           `json:"title"`
	Duration        int              `json:"duration"`
	TrackNumber     int              `json:"trackNumber"`
	VolumeNumber    int              `json:"volumeNumber"`
	ISRC            string           `json:"isrc"`
	Copyright       string           `json:"copyright"`
	AudioQuality    string           `json:"audioQuality"`
	AudioModes      []string         `json:"audioModes"`
	MediaMetadata   *tidalMediaMeta  `json:"mediaMetadata,omitempty"`
	Artist          tidalArtistBrief `json:"artist"`
	Artists         []tidalArtistRef `json:"artists"`
	Album           tidalAlbumBrief  `json:"album"`
	Explicit        bool             `json:"explicit"`
	Popularity      int              `json:"popularity"`
	StreamStartDate string           `json:"streamStartDate"`
}

type tidalArtistBrief struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

type tidalArtistRef struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // MAIN, FEATURED
	Picture string `json:"picture"`
}

type tidalAlbumBrief struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Cover string `json:"cover"`
}

type tidalAlbum struct {
	ID              int              `json:"id"`
	Title           string           `json:"title"`
	Duration        int              `json:"duration"`
	NumberOfTracks  int              `json:"numberOfTracks"`
	NumberOfVolumes int              `json:"numberOfVolumes"`
	ReleaseDate     string           `json:"releaseDate"`
	Copyright       string           `json:"copyright"`
	Type            string           `json:"type"`
	Cover           string           `json:"cover"`
	UPC             string           `json:"upc"`
	AudioQuality    string           `json:"audioQuality"`
	AudioModes      []string         `json:"audioModes"`
	MediaMetadata   *tidalMediaMeta  `json:"mediaMetadata,omitempty"`
	Artists         []tidalArtistRef `json:"artists"`
	Explicit        bool             `json:"explicit"`
	Popularity      int              `json:"popularity"`
	AllowStreaming  bool             `json:"allowStreaming"`
	StreamStartDate string           `json:"streamStartDate"`
}

type tidalAlbumDetail struct {
	tidalAlbum
	Tracks []tidalTrack `json:"tracks,omitempty"`
}

type tidalArtist struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

type tidalMediaMeta struct {
	Tags []string `json:"tags"` // e.g. ["LOSSLESS", "HIRES_LOSSLESS"]
}

type tidalPlaybackInfo struct {
	TrackID          int    `json:"trackId"`
	AudioQuality     string `json:"audioQuality"`
	AudioMode        string `json:"audioMode"`
	BitDepth         int    `json:"bitDepth"`
	SampleRate       int    `json:"sampleRate"`
	ManifestMimeType string `json:"manifestMimeType"`
	Manifest         string `json:"manifest"` // base64 encoded
}

type tidalLyricsResponse struct {
	TrackID        int    `json:"trackId"`
	LyricsProvider string `json:"lyricsProvider"`
	Lyrics         string `json:"lyrics"`
	Subtitles      string `json:"subtitles"` // synced lyrics in Tidal format
	IsRightToLeft  bool   `json:"isRightToLeft"`
}

// --- Conversion to dab types (shared with frontend) ---

func coverURL(coverID string) string {
	if coverID == "" {
		return ""
	}
	path := strings.ReplaceAll(coverID, "-", "/")
	return fmt.Sprintf("https://resources.tidal.com/images/%s/1280x1280.jpg", path)
}

func artistPictureURL(pictureID string) string {
	if pictureID == "" {
		return ""
	}
	path := strings.ReplaceAll(pictureID, "-", "/")
	return fmt.Sprintf("https://resources.tidal.com/images/%s/480x480.jpg", path)
}

func mainArtistName(artists []tidalArtistRef) string {
	for _, a := range artists {
		if a.Type == "MAIN" {
			return a.Name
		}
	}
	if len(artists) > 0 {
		return artists[0].Name
	}
	return ""
}

func toAudioQuality(t *tidalTrack) *dab.AudioQuality {
	tags := t.availableTags()
	if containsTag(tags, "HIRES_LOSSLESS") {
		return &dab.AudioQuality{MaxBitDepth: 24, MaxSamplingRate: 192, IsHiRes: true}
	}
	if containsTag(tags, "LOSSLESS") {
		return &dab.AudioQuality{MaxBitDepth: 16, MaxSamplingRate: 44.1, IsHiRes: false}
	}
	return nil
}

func (t *tidalTrack) availableTags() []string {
	if t.MediaMetadata != nil {
		return t.MediaMetadata.Tags
	}
	return nil
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

func (t *tidalTrack) toDABTrack() dab.Track {
	return dab.Track{
		ID:           dab.TrackID(fmt.Sprintf("%d", t.ID)),
		Title:        t.Title,
		Artist:       t.Artist.Name,
		ArtistID:     json.Number(fmt.Sprintf("%d", t.Artist.ID)),
		AlbumTitle:   t.Album.Title,
		AlbumCover:   coverURL(t.Album.Cover),
		AlbumID:      fmt.Sprintf("%d", t.Album.ID),
		Duration:     t.Duration,
		AudioQuality: toAudioQuality(t),
		AudioModes:   t.AudioModes,
	}
}

func (a *tidalAlbum) toDABAlbum() dab.Album {
	artist := mainArtistName(a.Artists)
	year := ""
	if len(a.ReleaseDate) >= 4 {
		year = a.ReleaseDate[:4]
	}
	return dab.Album{
		ID:          fmt.Sprintf("%d", a.ID),
		Title:       a.Title,
		Artist:      artist,
		Cover:       coverURL(a.Cover),
		ReleaseDate: a.ReleaseDate,
		Year:        year,
		Copyright:   a.Copyright,
		UPC:         a.UPC,
		TotalTracks: a.NumberOfTracks,
		TotalDiscs:  a.NumberOfVolumes,
		AudioModes:  a.AudioModes,
		MediaTags:   mediaTags(a.MediaMetadata),
	}
}

func mediaTags(m *tidalMediaMeta) []string {
	if m == nil {
		return nil
	}
	return m.Tags
}
