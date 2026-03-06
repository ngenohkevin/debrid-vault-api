package realdebrid

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/1.0/user" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing or wrong auth header")
		}
		json.NewEncoder(w).Encode(User{
			ID:         1,
			Username:   "testuser",
			Email:      "test@example.com",
			Premium:    1,
			Expiration: "2026-06-03T00:00:00.000Z",
			Type:       "premium",
		})
	}))
	defer server.Close()

	t.Run("handler returns correct user", func(t *testing.T) {
		req, _ := http.NewRequest("GET", server.URL+"/rest/1.0/user", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var user User
		json.NewDecoder(resp.Body).Decode(&user)
		if user.Username != "testuser" {
			t.Errorf("expected testuser, got %s", user.Username)
		}
		if user.Premium != 1 {
			t.Errorf("expected premium 1, got %d", user.Premium)
		}
	})
}

func TestListDownloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/1.0/downloads" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]Download{
			{
				ID:       "ABC123",
				Filename: "test-movie.mkv",
				Filesize: 1024 * 1024 * 500,
				Download: "https://example.com/dl/test.mkv",
			},
		})
	}))
	defer server.Close()

	t.Run("returns downloads list", func(t *testing.T) {
		req, _ := http.NewRequest("GET", server.URL+"/rest/1.0/downloads", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		var downloads []Download
		json.NewDecoder(resp.Body).Decode(&downloads)
		if len(downloads) != 1 {
			t.Fatalf("expected 1 download, got %d", len(downloads))
		}
		if downloads[0].Filename != "test-movie.mkv" {
			t.Errorf("expected test-movie.mkv, got %s", downloads[0].Filename)
		}
	})
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(APIError{
			ErrorCode: 8,
			Error:     "Permission denied",
		})
	}))
	defer server.Close()

	t.Run("handles API errors", func(t *testing.T) {
		req, _ := http.NewRequest("GET", server.URL+"/rest/1.0/user", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403, got %d", resp.StatusCode)
		}
	})
}
