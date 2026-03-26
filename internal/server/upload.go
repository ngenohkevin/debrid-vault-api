package server

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

type uploadResult struct {
	Artist string   `json:"artist"`
	Album  string   `json:"album"`
	Tracks int      `json:"tracks"`
	Path   string   `json:"path"`
	Files  []string `json:"files"`
}

const maxUploadSize = 2 << 30 // 2 GB

func (s *Server) musicUpload(c *gin.Context) {
	// Reject uploads over 2GB early
	if c.Request.ContentLength > maxUploadSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large (max 2 GB)"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadSize)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if err.Error() == "http: request body too large" {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large (max 2 GB)"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	defer file.Close()

	// Save to temp file on NVMe staging (not /tmp which is tmpfs/RAM)
	tmpDir := filepath.Join(s.cfg.DownloadDir, ".uploads")
	os.MkdirAll(tmpDir, 0755)
	tmpFile, err := os.CreateTemp(tmpDir, "music-upload-*"+filepath.Ext(header.Filename))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create temp file"})
		return
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save upload"})
		return
	}
	tmpFile.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	musicDir := s.cfg.MusicDir

	switch ext {
	case ".zip":
		result, err := handleZipUpload(tmpFile.Name(), musicDir)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, result)

	case ".flac", ".mp3", ".m4a", ".wav", ".alac", ".aac", ".ogg", ".opus":
		result, err := handleSingleUpload(tmpFile.Name(), header.Filename, musicDir)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, result)

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported file type: " + ext})
	}
}

func handleZipUpload(zipPath, musicDir string) (*uploadResult, error) {
	// Extract to temp dir (same parent as the zip to stay on NVMe)
	tmpDir, err := os.MkdirTemp(filepath.Dir(zipPath), "music-extract-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %v", err)
	}
	defer r.Close()

	var audioFiles []string
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Security: prevent path traversal
		name := filepath.Base(f.Name)
		destPath := filepath.Join(tmpDir, name)

		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.Create(destPath)
		if err != nil {
			rc.Close()
			continue
		}
		io.Copy(out, rc)
		out.Close()
		rc.Close()

		ext := strings.ToLower(filepath.Ext(name))
		if isAudioExt(ext) {
			audioFiles = append(audioFiles, destPath)
		}
	}

	if len(audioFiles) == 0 {
		return nil, fmt.Errorf("no audio files found in zip")
	}

	// Read metadata from the first audio file to determine artist/album
	meta := probeMetadata(audioFiles[0])
	artist := sanitizeFilename(meta["artist"])
	album := sanitizeFilename(meta["album"])

	if artist == "" {
		artist = "Unknown Artist"
	}
	if album == "" {
		album = "Unknown Album"
	}

	destDir := filepath.Join(musicDir, artist, album)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %v", err)
	}

	// Move all files (audio + cover art)
	var movedFiles []string
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(f.Name)
		srcPath := filepath.Join(tmpDir, name)
		if _, err := os.Stat(srcPath); err != nil {
			continue
		}
		destPath := filepath.Join(destDir, name)
		if err := moveFile(srcPath, destPath); err != nil {
			log.Printf("Failed to move %s: %v", name, err)
			continue
		}
		movedFiles = append(movedFiles, name)
	}

	return &uploadResult{
		Artist: artist,
		Album:  album,
		Tracks: len(audioFiles),
		Path:   destDir,
		Files:  movedFiles,
	}, nil
}

func handleSingleUpload(tmpPath, originalName, musicDir string) (*uploadResult, error) {
	meta := probeMetadata(tmpPath)
	artist := sanitizeFilename(meta["artist"])
	album := sanitizeFilename(meta["album"])

	if artist == "" {
		artist = "Unknown Artist"
	}
	if album == "" {
		album = "Singles"
	}

	destDir := filepath.Join(musicDir, artist, album)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %v", err)
	}

	destPath := filepath.Join(destDir, originalName)
	if err := moveFile(tmpPath, destPath); err != nil {
		return nil, fmt.Errorf("failed to move file: %v", err)
	}

	return &uploadResult{
		Artist: artist,
		Album:  album,
		Tracks: 1,
		Path:   destDir,
		Files:  []string{originalName},
	}, nil
}

// probeMetadata reads audio metadata using ffprobe.
func probeMetadata(filePath string) map[string]string {
	result := make(map[string]string)

	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", filePath)
	out, err := cmd.Output()
	if err != nil {
		return result
	}

	var data struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &data) == nil && data.Format.Tags != nil {
		// Normalize tag keys to lowercase
		for k, v := range data.Format.Tags {
			result[strings.ToLower(k)] = v
		}
	}

	return result
}

func isAudioExt(ext string) bool {
	switch ext {
	case ".flac", ".mp3", ".m4a", ".wav", ".alac", ".aac", ".ogg", ".opus", ".wma", ".aiff":
		return true
	}
	return false
}

func moveFile(src, dst string) error {
	// Try rename first (fast, same filesystem)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// Fallback: copy + delete
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}

	in.Close()
	return os.Remove(src)
}
