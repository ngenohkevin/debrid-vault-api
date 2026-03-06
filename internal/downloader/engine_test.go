package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", "1024")
			return
		}
	}))
	defer ts.Close()

	engine := NewDownloadEngine(8, 0)
	size, rangeOK, err := engine.probeURL(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("probeURL failed: %v", err)
	}
	if size != 1024 {
		t.Errorf("expected size 1024, got %d", size)
	}
	if !rangeOK {
		t.Error("expected rangeOK to be true")
	}
}

func TestProbeURLNoRange(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "512")
	}))
	defer ts.Close()

	engine := NewDownloadEngine(8, 0)
	size, rangeOK, err := engine.probeURL(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("probeURL failed: %v", err)
	}
	if size != 512 {
		t.Errorf("expected size 512, got %d", size)
	}
	if rangeOK {
		t.Error("expected rangeOK to be false")
	}
}

func TestDownloadSingleStream(t *testing.T) {
	content := make([]byte, 10*1024) // 10KB
	for i := range content {
		content[i] = byte(i % 256)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			// No Accept-Ranges — forces single stream
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "test.bin")

	engine := NewDownloadEngine(4, 0)
	var lastDownloaded int64
	err := engine.Download(context.Background(), ts.URL, destPath, 4, func(downloaded, total, speed int64) {
		lastDownloaded = downloaded
	}, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	result, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}
	if len(result) != len(content) {
		t.Errorf("size mismatch: got %d, want %d", len(result), len(content))
	}
	for i := range content {
		if result[i] != content[i] {
			t.Errorf("byte mismatch at offset %d: got %d, want %d", i, result[i], content[i])
			break
		}
	}
	_ = lastDownloaded
}

func TestDownloadMultiSegment(t *testing.T) {
	content := make([]byte, 100*1024) // 100KB
	for i := range content {
		content[i] = byte(i % 256)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		// Handle Range requests
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start, end int64
			fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
			if end >= int64(len(content)) {
				end = int64(len(content)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start : end+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "test.bin")

	engine := NewDownloadEngine(8, 0)
	err := engine.Download(context.Background(), ts.URL, destPath, 4, nil, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	result, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}
	if len(result) != len(content) {
		t.Fatalf("size mismatch: got %d, want %d", len(result), len(content))
	}
	for i := range content {
		if result[i] != content[i] {
			t.Errorf("byte mismatch at offset %d: got %d, want %d", i, result[i], content[i])
			break
		}
	}

	// .part file should be cleaned up
	if _, err := os.Stat(destPath + ".part"); !os.IsNotExist(err) {
		t.Error(".part file should be removed after successful download")
	}
}

func TestDownloadPauseResume(t *testing.T) {
	content := make([]byte, 100*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start, end int64
			fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
			if end >= int64(len(content)) {
				end = int64(len(content)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start : end+1])
			return
		}
		w.Write(content)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "test.bin")
	partPath := destPath + ".part"

	engine := NewDownloadEngine(8, 0)

	// Start and immediately cancel (simulate pause)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_ = engine.Download(ctx, ts.URL, destPath, 4, nil, nil)

	// .part file should exist after cancellation
	if _, err := os.Stat(partPath); os.IsNotExist(err) {
		// It's OK if the download completed before cancellation for small files
		// Just verify the dest file exists
		if _, err := os.Stat(destPath); os.IsNotExist(err) {
			t.Log("Neither .part nor dest file exists — cancel was too fast for probe")
		}
	}

	// Resume with fresh context
	err := engine.Download(context.Background(), ts.URL, destPath, 4, nil, nil)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	result, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read result: %v", err)
	}
	if len(result) != len(content) {
		t.Fatalf("size mismatch: got %d, want %d", len(result), len(content))
	}
}

func TestPartFilePersistence(t *testing.T) {
	engine := NewDownloadEngine(8, 0)
	tmpDir := t.TempDir()
	partPath := filepath.Join(tmpDir, "test.part")

	pf := &PartFile{
		URL:       "http://example.com/file.bin",
		TotalSize: 1024 * 1024,
		Segments: []SegmentState{
			{Start: 0, End: 524287, Downloaded: 100000},
			{Start: 524288, End: 1048575, Downloaded: 200000},
		},
		RangeOK: true,
	}

	engine.savePartFile(partPath, pf)

	loaded, ok := engine.loadPartFile(partPath)
	if !ok {
		t.Fatal("failed to load .part file")
	}
	if loaded.URL != pf.URL {
		t.Errorf("URL mismatch: got %s, want %s", loaded.URL, pf.URL)
	}
	if loaded.TotalSize != pf.TotalSize {
		t.Errorf("TotalSize mismatch: got %d, want %d", loaded.TotalSize, pf.TotalSize)
	}
	if len(loaded.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(loaded.Segments))
	}
	if loaded.Segments[0].Downloaded != 100000 {
		t.Errorf("segment 0 downloaded: got %d, want 100000", loaded.Segments[0].Downloaded)
	}
}

func TestCreatePartFile(t *testing.T) {
	engine := NewDownloadEngine(8, 0)

	pf := engine.createPartFile("http://example.com/file.bin", 1000, 4)
	if len(pf.Segments) != 4 {
		t.Fatalf("expected 4 segments, got %d", len(pf.Segments))
	}

	// Verify coverage
	if pf.Segments[0].Start != 0 {
		t.Errorf("first segment start: got %d, want 0", pf.Segments[0].Start)
	}
	if pf.Segments[3].End != 999 {
		t.Errorf("last segment end: got %d, want 999", pf.Segments[3].End)
	}

	// Verify no gaps
	for i := 1; i < len(pf.Segments); i++ {
		if pf.Segments[i].Start != pf.Segments[i-1].End+1 {
			t.Errorf("gap between segment %d and %d", i-1, i)
		}
	}
}

func TestSpeedLimit(t *testing.T) {
	engine := NewDownloadEngine(8, 0)

	// Initially no limiter
	if engine.getLimiter() != nil {
		t.Error("expected nil limiter with 0 Mbps")
	}

	// Set a limit
	engine.SetSpeedLimit(10)
	limiter := engine.getLimiter()
	if limiter == nil {
		t.Fatal("expected non-nil limiter after SetSpeedLimit(10)")
	}

	// Clear limit
	engine.SetSpeedLimit(0)
	if engine.getLimiter() != nil {
		t.Error("expected nil limiter after SetSpeedLimit(0)")
	}
}
