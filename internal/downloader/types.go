package downloader

import "time"

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
