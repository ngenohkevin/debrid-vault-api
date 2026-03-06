package media

import "time"

type MediaFile struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	IsDir    bool      `json:"isDir"`
	Category string    `json:"category"`
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
