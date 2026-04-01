package dab

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TrackMeta holds metadata to embed in a FLAC file.
type TrackMeta struct {
	Title        string
	Artist       string
	Album        string
	AlbumArtist  string
	TrackNumber  int
	TotalTracks  int
	DiscNumber   int
	Genre        string
	Year         string
	CoverURL     string
	Copyright    string
	Lyrics       string // plain lyrics (LYRICS vorbis comment)
	SyncedLyrics string // LRC format (written as .lrc sidecar)
	BitDepth     int
	SampleRate   int
	// MusicBrainz enrichment
	ISRC            string
	Label           string
	CatalogNumber   string
	Barcode         string
	TrackMBID       string
	ArtistMBID      string
	AlbumMBID       string
	AlbumArtistMBID string
	ReleaseGroupID  string
}

// TagFLAC embeds metadata and cover art into a FLAC file using ffmpeg.
func TagFLAC(filePath string, meta TrackMeta) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil // silently skip if ffmpeg not available
	}

	// Download cover art to temp file if available
	var coverPath string
	if meta.CoverURL != "" {
		var err error
		coverPath, err = downloadCover(meta.CoverURL)
		if err == nil {
			defer os.Remove(coverPath)
		}
	}

	// Build ffmpeg args: copy audio, add metadata
	tmpOut := filePath + ".tagged.flac"
	args := []string{"-y", "-i", filePath}

	if coverPath != "" {
		args = append(args, "-i", coverPath)
	}

	// Map streams
	args = append(args, "-map", "0:a")
	if coverPath != "" {
		args = append(args, "-map", "1:0")
	}

	args = append(args, "-c", "copy")

	if coverPath != "" {
		args = append(args, "-disposition:v:0", "attached_pic")
		args = append(args,
			"-metadata:s:v", "title=Album cover",
			"-metadata:s:v", "comment=Cover (front)",
		)
	}

	// Add metadata
	if meta.Title != "" {
		args = append(args, "-metadata", "TITLE="+meta.Title)
	}
	if meta.Artist != "" {
		args = append(args, "-metadata", "ARTIST="+meta.Artist)
	}
	if meta.Album != "" {
		args = append(args, "-metadata", "ALBUM="+meta.Album)
	}
	if meta.AlbumArtist != "" {
		args = append(args, "-metadata", "ALBUMARTIST="+meta.AlbumArtist)
	}
	if meta.TrackNumber > 0 {
		tn := fmt.Sprintf("%d", meta.TrackNumber)
		if meta.TotalTracks > 0 {
			tn = fmt.Sprintf("%d/%d", meta.TrackNumber, meta.TotalTracks)
		}
		args = append(args, "-metadata", "TRACKNUMBER="+tn)
	}
	if meta.DiscNumber > 0 {
		args = append(args, "-metadata", fmt.Sprintf("DISCNUMBER=%d", meta.DiscNumber))
	}
	if meta.Genre != "" {
		args = append(args, "-metadata", "GENRE="+meta.Genre)
	}
	if meta.Year != "" {
		args = append(args, "-metadata", "DATE="+meta.Year)
	}

	// MusicBrainz fields
	if meta.ISRC != "" {
		args = append(args, "-metadata", "ISRC="+meta.ISRC)
	}
	if meta.Label != "" {
		args = append(args, "-metadata", "LABEL="+meta.Label)
	}
	if meta.CatalogNumber != "" {
		args = append(args, "-metadata", "CATALOGNUMBER="+meta.CatalogNumber)
	}
	if meta.Barcode != "" {
		args = append(args, "-metadata", "BARCODE="+meta.Barcode)
	}
	if meta.TrackMBID != "" {
		args = append(args, "-metadata", "MUSICBRAINZ_TRACKID="+meta.TrackMBID)
	}
	if meta.ArtistMBID != "" {
		args = append(args, "-metadata", "MUSICBRAINZ_ARTISTID="+meta.ArtistMBID)
	}
	if meta.AlbumMBID != "" {
		args = append(args, "-metadata", "MUSICBRAINZ_ALBUMID="+meta.AlbumMBID)
	}
	if meta.AlbumArtistMBID != "" {
		args = append(args, "-metadata", "MUSICBRAINZ_ALBUMARTISTID="+meta.AlbumArtistMBID)
	}
	if meta.ReleaseGroupID != "" {
		args = append(args, "-metadata", "MUSICBRAINZ_RELEASEGROUPID="+meta.ReleaseGroupID)
	}
	if meta.Copyright != "" {
		args = append(args, "-metadata", "COPYRIGHT="+meta.Copyright)
	}
	if meta.Lyrics != "" {
		args = append(args, "-metadata", "LYRICS="+meta.Lyrics)
	}

	args = append(args, tmpOut)

	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpOut)
		return fmt.Errorf("ffmpeg failed: %v — %s", err, strings.TrimSpace(string(out)))
	}

	// Replace original with tagged file
	if err := os.Rename(tmpOut, filePath); err != nil {
		os.Remove(tmpOut)
		return fmt.Errorf("rename failed: %v", err)
	}

	// Write .lrc sidecar for synced lyrics
	if meta.SyncedLyrics != "" {
		lrcPath := strings.TrimSuffix(filePath, filepath.Ext(filePath)) + ".lrc"
		_ = os.WriteFile(lrcPath, []byte(meta.SyncedLyrics), 0644)
	}

	return nil
}

func downloadCover(coverURL string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(coverURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("cover download failed: %d", resp.StatusCode)
	}

	ext := ".jpg"
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "png") {
		ext = ".png"
	}

	tmp, err := os.CreateTemp("", "cover-*"+ext)
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}

	return tmp.Name(), nil
}

// CoverCachePath returns a path to cache album art for an album.
func CoverCachePath(destDir, albumID string) string {
	return filepath.Join(destDir, ".cover-"+albumID+".jpg")
}
