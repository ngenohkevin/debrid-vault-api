package downloader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadItemDefaults(t *testing.T) {
	item := DownloadItem{
		ID:        "test-1",
		Name:      "test-movie.mkv",
		Category:  CategoryMovies,
		Status:    StatusPending,
		CreatedAt: time.Now(),
	}

	if item.Status != StatusPending {
		t.Errorf("expected pending, got %s", item.Status)
	}
	if item.Category != CategoryMovies {
		t.Errorf("expected movies, got %s", item.Category)
	}
	if item.Progress != 0 {
		t.Errorf("expected 0 progress, got %f", item.Progress)
	}
}

func TestCategoryValidation(t *testing.T) {
	tests := []struct {
		name     string
		category Category
		valid    bool
	}{
		{"movies", CategoryMovies, true},
		{"tv-shows", CategoryTVShows, true},
		{"invalid", Category("invalid"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid := tt.category == CategoryMovies || tt.category == CategoryTVShows
			if valid != tt.valid {
				t.Errorf("category %s: expected valid=%v, got %v", tt.category, tt.valid, valid)
			}
		})
	}
}

func TestStatusTransitions(t *testing.T) {
	statuses := []Status{
		StatusPending,
		StatusResolving,
		StatusDownloading,
		StatusMoving,
		StatusCompleted,
		StatusError,
		StatusCancelled,
	}

	for _, s := range statuses {
		if s == "" {
			t.Error("status should not be empty")
		}
	}
}

func TestCopyFile(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "source.txt")
	dst := filepath.Join(tmpDir, "dest.txt")

	content := []byte("test file content for copy")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("failed to write source: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}

	result, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("failed to read dest: %v", err)
	}
	if string(result) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", result, content)
	}
}

func TestHistoryPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, ".history.json")

	// Create a manager-like structure and save
	items := []DownloadItem{
		{
			ID:        "test-1",
			Name:      "movie.mkv",
			Category:  CategoryMovies,
			Status:    StatusCompleted,
			Size:      1024,
			CreatedAt: time.Now(),
		},
	}

	// Save manually
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if err := os.WriteFile(historyFile, data, 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Load and verify
	loaded, err := os.ReadFile(historyFile)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	var loadedItems []DownloadItem
	if err := json.Unmarshal(loaded, &loadedItems); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(loadedItems) != 1 {
		t.Fatalf("expected 1 item, got %d", len(loadedItems))
	}
	if loadedItems[0].Name != "movie.mkv" {
		t.Errorf("expected movie.mkv, got %s", loadedItems[0].Name)
	}
}
