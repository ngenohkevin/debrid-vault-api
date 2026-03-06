package media

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ngenohkevin/debrid-vault-api/internal/config"
)

func setupTestDirs(t *testing.T) (*config.Config, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	moviesDir := filepath.Join(tmpDir, "movies")
	tvDir := filepath.Join(tmpDir, "tv-shows")
	downloadDir := filepath.Join(tmpDir, "staging")

	os.MkdirAll(moviesDir, 0755)
	os.MkdirAll(tvDir, 0755)
	os.MkdirAll(downloadDir, 0755)

	cfg := &config.Config{
		MoviesDir:   moviesDir,
		TVShowsDir:  tvDir,
		DownloadDir: downloadDir,
	}

	return cfg, func() { os.RemoveAll(tmpDir) }
}

func TestListMedia(t *testing.T) {
	cfg, cleanup := setupTestDirs(t)
	defer cleanup()
	lib := NewLibrary(cfg)

	// Create test files
	os.WriteFile(filepath.Join(cfg.MoviesDir, "movie1.mkv"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(cfg.MoviesDir, "movie2.mp4"), []byte("data2"), 0644)
	os.WriteFile(filepath.Join(cfg.TVShowsDir, "show1.mkv"), []byte("data3"), 0644)

	t.Run("list all", func(t *testing.T) {
		files, err := lib.ListMedia("")
		if err != nil {
			t.Fatalf("ListMedia failed: %v", err)
		}
		if len(files) != 3 {
			t.Errorf("expected 3 files, got %d", len(files))
		}
	})

	t.Run("list movies only", func(t *testing.T) {
		files, err := lib.ListMedia("movies")
		if err != nil {
			t.Fatalf("ListMedia failed: %v", err)
		}
		if len(files) != 2 {
			t.Errorf("expected 2 movies, got %d", len(files))
		}
		for _, f := range files {
			if f.Category != "movies" {
				t.Errorf("expected movies category, got %s", f.Category)
			}
		}
	})

	t.Run("list tv-shows only", func(t *testing.T) {
		files, err := lib.ListMedia("tv-shows")
		if err != nil {
			t.Fatalf("ListMedia failed: %v", err)
		}
		if len(files) != 1 {
			t.Errorf("expected 1 tv show, got %d", len(files))
		}
	})
}

func TestSearchMedia(t *testing.T) {
	cfg, cleanup := setupTestDirs(t)
	defer cleanup()
	lib := NewLibrary(cfg)

	os.WriteFile(filepath.Join(cfg.MoviesDir, "Inception.2010.mkv"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(cfg.MoviesDir, "Interstellar.2014.mkv"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(cfg.TVShowsDir, "Breaking.Bad.S01.mkv"), []byte("data"), 0644)

	t.Run("search by name", func(t *testing.T) {
		results, err := lib.SearchMedia("inception")
		if err != nil {
			t.Fatalf("SearchMedia failed: %v", err)
		}
		if len(results) != 1 {
			t.Errorf("expected 1 result, got %d", len(results))
		}
	})

	t.Run("search partial", func(t *testing.T) {
		results, err := lib.SearchMedia("inter")
		if err != nil {
			t.Fatalf("SearchMedia failed: %v", err)
		}
		if len(results) != 1 {
			t.Errorf("expected 1 result, got %d", len(results))
		}
	})

	t.Run("search no results", func(t *testing.T) {
		results, err := lib.SearchMedia("nonexistent")
		if err != nil {
			t.Fatalf("SearchMedia failed: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results, got %d", len(results))
		}
	})
}

func TestDeleteMedia(t *testing.T) {
	cfg, cleanup := setupTestDirs(t)
	defer cleanup()
	lib := NewLibrary(cfg)

	testFile := filepath.Join(cfg.MoviesDir, "deleteme.mkv")
	os.WriteFile(testFile, []byte("data"), 0644)

	t.Run("delete valid file", func(t *testing.T) {
		err := lib.DeleteMedia(testFile)
		if err != nil {
			t.Fatalf("DeleteMedia failed: %v", err)
		}
		if _, err := os.Stat(testFile); !os.IsNotExist(err) {
			t.Error("file should have been deleted")
		}
	})

	t.Run("reject path traversal", func(t *testing.T) {
		err := lib.DeleteMedia("/etc/passwd")
		if err == nil {
			t.Error("should reject paths outside media directories")
		}
	})

	t.Run("reject relative path traversal", func(t *testing.T) {
		err := lib.DeleteMedia(filepath.Join(cfg.MoviesDir, "..", "..", "etc", "passwd"))
		if err == nil {
			t.Error("should reject directory traversal")
		}
	})
}

func TestGetStorageInfo(t *testing.T) {
	cfg, cleanup := setupTestDirs(t)
	defer cleanup()
	lib := NewLibrary(cfg)

	info, err := lib.GetStorageInfo()
	if err != nil {
		t.Fatalf("GetStorageInfo failed: %v", err)
	}

	if info.NVMe.Total == 0 {
		t.Error("NVMe total should not be 0")
	}
	if info.External.Total == 0 {
		t.Error("External total should not be 0")
	}
	if info.NVMe.Percent < 0 || info.NVMe.Percent > 100 {
		t.Errorf("NVMe percent out of range: %f", info.NVMe.Percent)
	}
}
