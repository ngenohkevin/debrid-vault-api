package dab

import (
	"encoding/json"
	"fmt"
)

type SearchResult struct {
	Tracks  []Track  `json:"tracks"`
	Albums  []Album  `json:"albums"`
	Artists []Artist `json:"artists"`
}

type Track struct {
	ID           TrackID       `json:"id"`
	Title        string        `json:"title"`
	Artist       string        `json:"artist"`
	ArtistID     json.Number   `json:"artistId"`
	AlbumTitle   string        `json:"albumTitle"`
	AlbumCover   string        `json:"albumCover"`
	AlbumID      string        `json:"albumId"`
	ReleaseDate  string        `json:"releaseDate"`
	Genre        string        `json:"genre"`
	Duration     int           `json:"duration"`
	AudioQuality *AudioQuality `json:"audioQuality,omitempty"`
}

type TrackDetail struct {
	ID          TrackID     `json:"id"`
	Title       string      `json:"title"`
	Artist      string      `json:"artist"`
	ArtistID    json.Number `json:"artistId"`
	AlbumCover  string      `json:"albumCover"`
	ReleaseDate string      `json:"releaseDate"`
	Duration    int         `json:"duration"`
	Album       string      `json:"album"`
	AlbumArtist string      `json:"albumArtist"`
	Genre       string      `json:"genre"`
	TrackNumber int         `json:"trackNumber"`
	DiscNumber  int         `json:"discNumber"`
	Composer    string      `json:"composer"`
	ISRC        string      `json:"isrc"`
	Copyright   string      `json:"copyright"`
	AlbumID     string      `json:"albumId"`
}

type AudioQuality struct {
	MaxBitDepth     int     `json:"maximumBitDepth"`
	MaxSamplingRate float64 `json:"maximumSamplingRate"`
	IsHiRes         bool    `json:"isHiRes"`
}

type Album struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Artist      string      `json:"artist"`
	ArtistID    json.Number `json:"artistId,omitempty"`
	Cover       string      `json:"cover"`
	ReleaseDate string      `json:"releaseDate"`
	Genre       string      `json:"genre"`
	Type        string      `json:"type,omitempty"`
	Label       string      `json:"label,omitempty"`
	UPC         string      `json:"upc,omitempty"`
	Copyright   string      `json:"copyright,omitempty"`
	Year        string      `json:"year,omitempty"`
	TotalTracks int         `json:"totalTracks,omitempty"`
	TotalDiscs  int         `json:"totalDiscs,omitempty"`
	Tracks      []Track     `json:"tracks,omitempty"`
}

type Artist struct {
	ID          json.Number `json:"id"`
	Name        string      `json:"name"`
	Picture     string      `json:"picture"`
	AlbumsCount int         `json:"albumsCount"`
}

type DiscographyResult struct {
	Artist Artist  `json:"artist"`
	Albums []Album `json:"albums"`
}

type Lyrics struct {
	Lyrics   string `json:"lyrics"`
	Unsynced bool   `json:"unsynced"`
}

// TrackID handles the polymorphic track ID (can be number or string in JSON).
type TrackID string

func (t *TrackID) UnmarshalJSON(data []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*t = TrackID(s)
		return nil
	}
	// Try number (int or float)
	var f float64
	if err := json.Unmarshal(data, &f); err == nil {
		*t = TrackID(fmt.Sprintf("%.0f", f))
		return nil
	}
	return fmt.Errorf("cannot unmarshal track ID: %s", string(data))
}

func (t TrackID) String() string {
	return string(t)
}

func (t TrackID) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(t))
}
