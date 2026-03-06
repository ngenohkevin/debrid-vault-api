package downloader

import (
	"regexp"
	"time"
)

type Status string

const (
	StatusPending     Status = "pending"
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

type DownloadItem struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Category    Category   `json:"category"`
	Status      Status     `json:"status"`
	Progress    float64    `json:"progress"`
	Speed       int64      `json:"speed"`
	Size        int64      `json:"size"`
	Downloaded  int64      `json:"downloaded"`
	ETA         int64      `json:"eta"`
	Error       string     `json:"error,omitempty"`
	Source      string     `json:"source"`
	Folder      string     `json:"folder,omitempty"`
	GroupID     string     `json:"groupId,omitempty"`
	GroupName   string     `json:"groupName,omitempty"`
	DownloadURL string     `json:"downloadUrl,omitempty"`
	FilePath    string     `json:"filePath,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type Event struct {
	Type string       `json:"type"`
	Data DownloadItem `json:"data"`
}
