package downloader

import (
	"regexp"
	"strings"
	"time"
)

type Status string

const (
	StatusPending     Status = "pending"
	StatusQueued      Status = "queued"
	StatusResolving   Status = "resolving"
	StatusDownloading Status = "downloading"
	StatusMoving      Status = "moving"
	StatusCompleted   Status = "completed"
	StatusPaused      Status = "paused"
	StatusError       Status = "error"
	StatusCancelled   Status = "cancelled"
)

type Category string

const (
	CategoryMovies  Category = "movies"
	CategoryTVShows Category = "tv-shows"
)

// tvShowPattern matches common TV episode naming: S01E01, S1E1, 1x01, etc.
var tvShowPattern = regexp.MustCompile(`(?i)(S\d{1,2}E\d{1,2}|S\d{1,2}\.E\d{1,2}|\d{1,2}x\d{2}|[._\s]E\d{2}[._\s]|Season[._\s]?\d|COMPLETE|MINISERIES)`)

// DetectCategory determines if a filename looks like a TV show or movie.
func DetectCategory(filename string) Category {
	if tvShowPattern.MatchString(filename) {
		return CategoryTVShows
	}
	return CategoryMovies
}

type SubtitleStatus string

const (
	SubtitleLikely   SubtitleStatus = "likely"
	SubtitleUnlikely SubtitleStatus = "unlikely"
	SubtitleUnknown  SubtitleStatus = "unknown"
	SubtitleConfirmed SubtitleStatus = "confirmed"
	SubtitleNone     SubtitleStatus = "none"
)

// subtitleIndicators are filename patterns that suggest embedded subtitles.
var subtitleIndicators = regexp.MustCompile(`(?i)(\.mkv$|MULTI[._\s]?SUB|SUBBED|[._\s]SUBS[._\s]|SUBTITLES|DUAL[._\s]AUDIO|MULTI[._\s]|ESub|HCSub|HC[._\s]SUB|REMUX|BluRay.*REMUX)`)

// subtitleUnlikelyIndicators are patterns that suggest no embedded subtitles.
var subtitleUnlikelyIndicators = regexp.MustCompile(`(?i)(\.mp4$|HDCAM|CAM[._\s]|TS[._\s]|TELESYNC|WEBRip.*x264|YIFY|YTS)`)

// DetectSubtitleStatus analyzes a filename to predict embedded subtitle likelihood.
func DetectSubtitleStatus(filename string) SubtitleStatus {
	if filename == "" {
		return SubtitleUnknown
	}
	if subtitleIndicators.MatchString(filename) {
		return SubtitleLikely
	}
	if subtitleUnlikelyIndicators.MatchString(filename) {
		return SubtitleUnlikely
	}
	// MKV container is the strongest single indicator
	if strings.HasSuffix(strings.ToLower(filename), ".mkv") {
		return SubtitleLikely
	}
	return SubtitleUnknown
}

type DownloadItem struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Category       Category       `json:"category"`
	Status         Status         `json:"status"`
	Progress       float64        `json:"progress"`
	Speed          int64          `json:"speed"`
	Size           int64          `json:"size"`
	Downloaded     int64          `json:"downloaded"`
	ETA            int64          `json:"eta"`
	Error          string         `json:"error,omitempty"`
	Source         string         `json:"source"`
	Folder         string         `json:"folder,omitempty"`
	GroupID        string         `json:"groupId,omitempty"`
	GroupName      string         `json:"groupName,omitempty"`
	DownloadURL    string         `json:"downloadUrl,omitempty"`
	FilePath       string         `json:"filePath,omitempty"`
	SubtitleStatus SubtitleStatus `json:"subtitleStatus"`
	CreatedAt      time.Time      `json:"createdAt"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty"`
}

type Event struct {
	Type string       `json:"type"`
	Data DownloadItem `json:"data"`
}
