package media

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ngenohkevin/debrid-vault-api/internal/config"
)

type Library struct {
	cfg *config.Config
}

func NewLibrary(cfg *config.Config) *Library {
	return &Library{cfg: cfg}
}

func (l *Library) ListMedia(category string) ([]MediaFile, error) {
	var dirs []struct {
		path string
		cat  string
	}

	switch category {
	case "movies":
		dirs = append(dirs, struct{ path, cat string }{l.cfg.MoviesDir, "movies"})
	case "tv-shows":
		dirs = append(dirs, struct{ path, cat string }{l.cfg.TVShowsDir, "tv-shows"})
	default:
		dirs = append(dirs,
			struct{ path, cat string }{l.cfg.MoviesDir, "movies"},
			struct{ path, cat string }{l.cfg.TVShowsDir, "tv-shows"},
		)
	}

	var files []MediaFile
	for _, d := range dirs {
		entries, err := os.ReadDir(d.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			files = append(files, MediaFile{
				Name:     entry.Name(),
				Path:     filepath.Join(d.path, entry.Name()),
				Size:     info.Size(),
				ModTime:  info.ModTime(),
				IsDir:    entry.IsDir(),
				Category: d.cat,
			})
		}
	}
	return files, nil
}

func (l *Library) SearchMedia(query string) ([]MediaFile, error) {
	all, err := l.ListMedia("")
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var results []MediaFile
	for _, f := range all {
		if strings.Contains(strings.ToLower(f.Name), q) {
			results = append(results, f)
		}
	}
	return results, nil
}

func (l *Library) DeleteMedia(path string) error {
	// Security: ensure path is within allowed directories
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path")
	}
	moviesAbs, _ := filepath.Abs(l.cfg.MoviesDir)
	tvAbs, _ := filepath.Abs(l.cfg.TVShowsDir)

	if !strings.HasPrefix(absPath, moviesAbs) && !strings.HasPrefix(absPath, tvAbs) {
		return fmt.Errorf("path not in media directories")
	}

	return os.RemoveAll(absPath)
}

func (l *Library) GetStorageInfo() (*StorageInfo, error) {
	nvme, err := getDiskUsage(l.cfg.DownloadDir)
	if err != nil {
		nvme = DiskUsage{}
	}

	external, err := getDiskUsage(l.cfg.MoviesDir)
	if err != nil {
		external = DiskUsage{}
	}

	return &StorageInfo{
		NVMe:     nvme,
		External: external,
	}, nil
}

func getDiskUsage(path string) (DiskUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskUsage{}, err
	}

	total := stat.Blocks * uint64(stat.Bsize)
	available := stat.Bavail * uint64(stat.Bsize)
	used := total - available
	var percent float64
	if total > 0 {
		percent = float64(used) / float64(total) * 100
	}

	return DiskUsage{
		Total:     total,
		Used:      used,
		Available: available,
		Percent:   percent,
	}, nil
}
