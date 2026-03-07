package media

import "time"

type SubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
	Codec    string `json:"codec"`
	Forced   bool   `json:"forced,omitempty"`
}

type MediaFile struct {
	Name           string          `json:"name"`
	Path           string          `json:"path"`
	Size           int64           `json:"size"`
	ModTime        time.Time       `json:"modTime"`
	IsDir          bool            `json:"isDir"`
	Category       string          `json:"category"`
	HasSubtitles   *bool           `json:"hasSubtitles,omitempty"`
	SubtitleTracks []SubtitleTrack `json:"subtitleTracks,omitempty"`
}

type DiskUsage struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
}

type StorageInfo struct {
	NVMe     DiskUsage `json:"nvme"`
	External DiskUsage `json:"external"`
}
