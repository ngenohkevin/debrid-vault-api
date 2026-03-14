package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngenohkevin/debrid-vault-api/internal/config"
	"github.com/ngenohkevin/debrid-vault-api/internal/debrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
	"github.com/ngenohkevin/debrid-vault-api/internal/realdebrid"
)

func setupTestServer() *Server {
	cfg := &config.Config{
		Port:                   "6501",
		RDApiKey:               "test-key",
		DownloadDir:            "/tmp/test-downloads",
		MoviesDir:              "/tmp/test-movies",
		TVShowsDir:             "/tmp/test-tvshows",
		MusicDir:               "/tmp/test-music",
		AllowedOrigins:         "*",
		MaxConcurrentDownloads: 4,
		MaxSegmentsPerFile:     8,
	}

	providers := map[string]debrid.Provider{
		"realdebrid": realdebrid.NewClient("test-key"),
	}
	dlManager := downloader.NewManager(cfg, providers)
	scheduler := downloader.NewScheduler(dlManager)
	library := media.NewLibrary(cfg)

	return New(cfg, providers, dlManager, scheduler, library)
}

func TestHealthCheck(t *testing.T) {
	srv := setupTestServer()
	router := srv.Router()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Errorf("expected ok status, got %s", resp["status"])
	}
	if resp["service"] != "debrid-vault" {
		t.Errorf("expected debrid-vault service, got %s", resp["service"])
	}
}

func TestListDownloadsEmpty(t *testing.T) {
	srv := setupTestServer()
	router := srv.Router()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/downloads", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var downloads []downloader.DownloadItem
	json.Unmarshal(w.Body.Bytes(), &downloads)
	if len(downloads) != 0 {
		t.Errorf("expected empty downloads, got %d", len(downloads))
	}
}

func TestStartDownloadBadRequest(t *testing.T) {
	srv := setupTestServer()
	router := srv.Router()

	tests := []struct {
		name string
		body string
		code int
	}{
		{"empty body", `{}`, http.StatusBadRequest},
		{"missing category", `{"source":"magnet:?xt=test"}`, http.StatusBadRequest},
		{"missing source", `{"category":"movies"}`, http.StatusBadRequest},
		{"invalid category", `{"source":"magnet:?xt=test","category":"invalid"}`, http.StatusBadRequest},
		{"invalid source", `{"source":"not-a-url","category":"movies"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/api/downloads", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			if w.Code != tt.code {
				t.Errorf("expected %d, got %d: %s", tt.code, w.Code, w.Body.String())
			}
		})
	}
}

func TestGetDownloadNotFound(t *testing.T) {
	srv := setupTestServer()
	router := srv.Router()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/downloads/nonexistent", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCancelDownloadNotFound(t *testing.T) {
	srv := setupTestServer()
	router := srv.Router()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/downloads/nonexistent", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSearchLibraryMissingQuery(t *testing.T) {
	srv := setupTestServer()
	router := srv.Router()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/library/search", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPIKeyMiddleware(t *testing.T) {
	cfg := &config.Config{
		Port:                   "6501",
		RDApiKey:               "test-key",
		DownloadDir:            "/tmp/test-downloads",
		MoviesDir:              "/tmp/test-movies",
		TVShowsDir:             "/tmp/test-tvshows",
		MusicDir:               "/tmp/test-music",
		AllowedOrigins:         "*",
		APIKey:                 "secret-key",
		MaxConcurrentDownloads: 4,
		MaxSegmentsPerFile:     8,
	}

	providers := map[string]debrid.Provider{
		"realdebrid": realdebrid.NewClient("test-key"),
	}
	dlManager := downloader.NewManager(cfg, providers)
	scheduler := downloader.NewScheduler(dlManager)
	library := media.NewLibrary(cfg)
	srv := New(cfg, providers, dlManager, scheduler, library)
	router := srv.Router()

	t.Run("health check bypasses auth", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/health", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("missing key returns 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/downloads", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("wrong key returns 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/downloads", nil)
		req.Header.Set("X-API-Key", "wrong-key")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("correct key passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/downloads", nil)
		req.Header.Set("X-API-Key", "secret-key")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("key via query param", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/downloads?api_key=secret-key", nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}
