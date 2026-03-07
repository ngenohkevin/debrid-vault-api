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
			mf := MediaFile{
				Name:     entry.Name(),
				Path:     filepath.Join(d.path, entry.Name()),
				Size:     info.Size(),
				ModTime:  info.ModTime(),
				IsDir:    entry.IsDir(),
				Category: d.cat,
			}
			if !entry.IsDir() {
				hasSubs, tracks := ProbeSubtitles(mf.Path)
				mf.HasSubtitles = &hasSubs
				mf.SubtitleTracks = tracks
			}
			files = append(files, mf)
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

func (l *Library) MoveMedia(path string, toCategory string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path")
	}
	moviesAbs, _ := filepath.Abs(l.cfg.MoviesDir)
	tvAbs, _ := filepath.Abs(l.cfg.TVShowsDir)

	if !strings.HasPrefix(absPath, moviesAbs) && !strings.HasPrefix(absPath, tvAbs) {
		return "", fmt.Errorf("path not in media directories")
	}

	var destDir string
	switch toCategory {
	case "movies":
		destDir = l.cfg.MoviesDir
	case "tv-shows":
		destDir = l.cfg.TVShowsDir
	default:
		return "", fmt.Errorf("invalid category: %s", toCategory)
	}

	name := filepath.Base(absPath)
	destPath := filepath.Join(destDir, name)

	// Don't move to same location
	if absPath == destPath {
		return "", fmt.Errorf("file is already in %s", toCategory)
	}

	if err := os.Rename(absPath, destPath); err != nil {
		return "", fmt.Errorf("failed to move: %w", err)
	}

	return destPath, nil
}

// ProbeSubtitlesForPath probes a file or all video files in a directory for subtitles.
func (l *Library) ProbeSubtitlesForPath(path string) ([]MediaFile, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path")
	}
	moviesAbs, _ := filepath.Abs(l.cfg.MoviesDir)
	tvAbs, _ := filepath.Abs(l.cfg.TVShowsDir)

	if !strings.HasPrefix(absPath, moviesAbs) && !strings.HasPrefix(absPath, tvAbs) {
		return nil, fmt.Errorf("path not in media directories")
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path not found")
	}

	var results []MediaFile
	if !info.IsDir() {
		hasSubs, tracks := ProbeSubtitles(absPath)
		results = append(results, MediaFile{
			Name:           info.Name(),
			Path:           absPath,
			Size:           info.Size(),
			ModTime:        info.ModTime(),
			HasSubtitles:   &hasSubs,
			SubtitleTracks: tracks,
		})
		return results, nil
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}
		fullPath := filepath.Join(absPath, entry.Name())
		hasSubs, tracks := ProbeSubtitles(fullPath)
		if hasSubs {
			results = append(results, MediaFile{
				Name:           entry.Name(),
				Path:           fullPath,
				Size:           entryInfo.Size(),
				ModTime:        entryInfo.ModTime(),
				HasSubtitles:   &hasSubs,
				SubtitleTracks: tracks,
			})
		}
	}
	return results, nil
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
