package media

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ffprobeOutput is the JSON structure returned by ffprobe.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
}

type ffprobeStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Tags        map[string]string `json:"tags,omitempty"`
	Disposition map[string]int    `json:"disposition,omitempty"`
}

// subtitleCache caches ffprobe results keyed by filepath.
// Entries are invalidated if the file's mod time changes.
type subtitleCache struct {
	mu      sync.RWMutex
	entries map[string]subtitleCacheEntry
}

type subtitleCacheEntry struct {
	modTime time.Time
	hasSubs bool
	tracks  []SubtitleTrack
}

var subCache = &subtitleCache{
	entries: make(map[string]subtitleCacheEntry),
}

// videoExtensions are file extensions we'll probe for subtitles.
var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".webm": true, ".m4v": true,
}

// ProbeSubtitles runs ffprobe on a video file and returns subtitle track info.
func ProbeSubtitles(path string) (bool, []SubtitleTrack) {
	ext := strings.ToLower(filepath.Ext(path))
	if !videoExtensions[ext] {
		return false, nil
	}

	// Check cache
	subCache.mu.RLock()
	if entry, ok := subCache.entries[path]; ok {
		subCache.mu.RUnlock()
		return entry.hasSubs, entry.tracks
	}
	subCache.mu.RUnlock()

	// Run ffprobe with a 5-second timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-select_streams", "s",
		"-show_entries", "stream=index,codec_name:stream_tags=language,title:stream_disposition=forced",
		"-of", "json",
		path,
	)

	out, err := cmd.Output()
	if err != nil {
		return false, nil
	}

	var result ffprobeOutput
	if err := json.Unmarshal(out, &result); err != nil {
		return false, nil
	}

	var tracks []SubtitleTrack
	for _, s := range result.Streams {
		if s.CodecType != "" && s.CodecType != "subtitle" {
			continue
		}
		track := SubtitleTrack{
			Index:    s.Index,
			Codec:    s.CodecName,
			Language: s.Tags["language"],
			Title:    s.Tags["title"],
		}
		if s.Disposition != nil && s.Disposition["forced"] == 1 {
			track.Forced = true
		}
		tracks = append(tracks, track)
	}

	hasSubs := len(tracks) > 0

	// Cache the result
	subCache.mu.Lock()
	subCache.entries[path] = subtitleCacheEntry{
		hasSubs: hasSubs,
		tracks:  tracks,
	}
	subCache.mu.Unlock()

	return hasSubs, tracks
}

// probeFirstVideoInDir finds the first video file in a directory and probes it.
// Returns nil for hasSubs if no video file is found.
func probeFirstVideoInDir(dir string) (*bool, []SubtitleTrack) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if videoExtensions[ext] {
			hasSubs, tracks := ProbeSubtitles(filepath.Join(dir, entry.Name()))
			return &hasSubs, tracks
		}
	}
	return nil, nil
}

// InvalidateSubtitleCache removes a path from the cache.
func InvalidateSubtitleCache(path string) {
	subCache.mu.Lock()
	delete(subCache.entries, path)
	subCache.mu.Unlock()
}
