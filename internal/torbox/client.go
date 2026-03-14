package torbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ngenohkevin/debrid-vault-api/internal/debrid"
)

const baseURL = "https://api.torbox.app/v1/api"

type cacheEntry[T any] struct {
	data      T
	expiresAt time.Time
}

type Client struct {
	apiKey     string
	httpClient *http.Client

	mu            sync.RWMutex
	torrentsCache *cacheEntry[[]debrid.Torrent]
	infoCache     map[string]*cacheEntry[debrid.Torrent]
	cacheTTL      time.Duration
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		infoCache: make(map[string]*cacheEntry[debrid.Torrent]),
		cacheTTL:  5 * time.Minute,
	}
}

func (c *Client) Name() string { return "TorBox" }

func (c *Client) InvalidateCache() {
	c.mu.Lock()
	c.torrentsCache = nil
	c.infoCache = make(map[string]*cacheEntry[debrid.Torrent])
	c.mu.Unlock()
}

func (c *Client) doRequest(method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		var apiResp tbResponse[any]
		if json.Unmarshal(data, &apiResp) == nil && apiResp.Detail != "" {
			return nil, fmt.Errorf("TorBox API error %d: %s", resp.StatusCode, apiResp.Detail)
		}
		return nil, fmt.Errorf("TorBox API HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}

func (c *Client) get(path string) ([]byte, error) {
	return c.doRequest("GET", path, nil, "")
}

func (c *Client) postJSON(path string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.doRequest("POST", path, bytes.NewReader(data), "application/json")
}

func (c *Client) postMultipart(path string, fields map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	w.Close()
	return c.doRequest("POST", path, &buf, w.FormDataContentType())
}

func (c *Client) GetUser() (*debrid.User, error) {
	data, err := c.get("/user/me")
	if err != nil {
		return nil, err
	}
	var resp tbResponse[tbUser]
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("TorBox: %s", resp.Detail)
	}
	u := resp.Data
	return &debrid.User{
		ID:         u.ID,
		Username:   u.Email,
		Email:      u.Email,
		Premium:    u.Plan,
		Expiration: u.Expiry,
		Type:       u.PlanName,
	}, nil
}

func (c *Client) ListTorrents() ([]debrid.Torrent, error) {
	c.mu.RLock()
	if c.torrentsCache != nil && time.Now().Before(c.torrentsCache.expiresAt) {
		result := c.torrentsCache.data
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	data, err := c.get("/torrents/mylist?bypass_cache=true")
	if err != nil {
		return nil, err
	}
	var resp tbResponse[[]tbTorrent]
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	torrents := make([]debrid.Torrent, 0, len(resp.Data))
	for _, t := range resp.Data {
		torrents = append(torrents, convertTorrent(t))
	}

	c.mu.Lock()
	c.torrentsCache = &cacheEntry[[]debrid.Torrent]{data: torrents, expiresAt: time.Now().Add(c.cacheTTL)}
	c.mu.Unlock()

	return torrents, nil
}

func (c *Client) GetTorrentInfo(id string) (*debrid.Torrent, error) {
	c.mu.RLock()
	if entry, ok := c.infoCache[id]; ok && time.Now().Before(entry.expiresAt) {
		result := entry.data
		c.mu.RUnlock()
		return &result, nil
	}
	c.mu.RUnlock()

	data, err := c.get("/torrents/mylist?bypass_cache=true&id=" + id)
	if err != nil {
		return nil, err
	}
	var resp tbResponse[tbTorrent]
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	torrent := convertTorrent(resp.Data)

	c.mu.Lock()
	c.infoCache[id] = &cacheEntry[debrid.Torrent]{data: torrent, expiresAt: time.Now().Add(c.cacheTTL)}
	c.mu.Unlock()

	return &torrent, nil
}

func (c *Client) AddMagnet(magnet string) (*debrid.AddMagnetResponse, error) {
	data, err := c.postMultipart("/torrents/createtorrent", map[string]string{
		"magnet": magnet,
		"seed":   "3", // don't seed
	})
	if err != nil {
		return nil, err
	}
	var resp tbResponse[tbCreateTorrent]
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("TorBox: %s", resp.Detail)
	}
	return &debrid.AddMagnetResponse{
		ID:  strconv.Itoa(resp.Data.TorrentID),
		URI: "",
	}, nil
}

func (c *Client) SelectFiles(torrentID string, files string) error {
	// TorBox auto-selects all files, no action needed
	return nil
}

func (c *Client) DeleteTorrent(id string) error {
	intID, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("invalid torrent ID: %s", id)
	}
	_, err = c.postJSON("/torrents/controltorrent", map[string]any{
		"torrent_id": intID,
		"operation":  "delete",
	})
	return err
}

func (c *Client) UnrestrictLink(link string) (*debrid.UnrestrictedLink, error) {
	// TorBox uses requestdl with torrent_id and file_id instead of unrestricting links.
	// For RD-style links (https://real-debrid.com/d/...), this won't apply.
	// For TorBox, we use the web download endpoint for direct URLs.
	// When called from the magnet flow, the link is actually a TorBox download request.

	// Check if this is a TorBox download request (format: "tb://torrent_id/file_id")
	if len(link) > 5 && link[:5] == "tb://" {
		path := link[5:]
		data, err := c.get("/torrents/requestdl?token=" + c.apiKey + "&" + path)
		if err != nil {
			return nil, err
		}
		// Response data field is the download URL string directly
		var resp tbResponse[string]
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, err
		}
		if !resp.Success {
			return nil, fmt.Errorf("TorBox: %s", resp.Detail)
		}
		return &debrid.UnrestrictedLink{
			Download: resp.Data,
			Filename: "download",
		}, nil
	}

	// For direct URLs, use web download
	data, err := c.postMultipart("/webdl/createwebdownload", map[string]string{
		"link": link,
	})
	if err != nil {
		return nil, err
	}
	var resp tbResponse[tbCreateTorrent]
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &debrid.UnrestrictedLink{
		ID:       strconv.Itoa(resp.Data.TorrentID),
		Filename: resp.Data.Name,
		Download: link,
	}, nil
}

func (c *Client) ListDownloads(limit int) ([]debrid.Download, error) {
	// TorBox doesn't have a separate downloads list like RD.
	// Return torrents that are completed as downloads.
	torrents, err := c.ListTorrents()
	if err != nil {
		return nil, err
	}
	var downloads []debrid.Download
	for _, t := range torrents {
		if t.Status == "downloaded" {
			downloads = append(downloads, debrid.Download{
				ID:       t.ID,
				Filename: t.Filename,
				Filesize: t.Bytes,
				Link:     t.ID,
				Download: t.ID,
			})
		}
	}
	if limit > 0 && len(downloads) > limit {
		downloads = downloads[:limit]
	}
	return downloads, nil
}

// RequestDownloadLink gets a direct download URL for a specific file in a torrent.
func (c *Client) RequestDownloadLink(torrentID int, fileID int) (string, error) {
	path := fmt.Sprintf("/torrents/requestdl?token=%s&torrent_id=%d&file_id=%d", c.apiKey, torrentID, fileID)
	data, err := c.get(path)
	if err != nil {
		return "", err
	}
	var resp tbResponse[string]
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("TorBox: %s", resp.Detail)
	}
	return resp.Data, nil
}

func convertTorrent(t tbTorrent) debrid.Torrent {
	id := strconv.Itoa(t.ID)
	status := mapStatus(t.DownloadState)

	// Build links from files (each file becomes a downloadable link)
	links := make([]string, 0, len(t.Files))
	for _, f := range t.Files {
		// Encode as tb:// URI so UnrestrictLink knows how to handle it
		links = append(links, fmt.Sprintf("tb://torrent_id=%d&file_id=%d", t.ID, f.ID))
	}

	// Convert files
	files := make([]debrid.TorrentFile, 0, len(t.Files))
	for _, f := range t.Files {
		files = append(files, debrid.TorrentFile{
			ID:       f.ID,
			Path:     f.Name,
			Bytes:    f.Size,
			Selected: 1,
		})
	}

	return debrid.Torrent{
		ID:       id,
		Filename: t.Name,
		Hash:     t.Hash,
		Bytes:    t.Size,
		Progress: t.Progress,
		Status:   status,
		Added:    t.CreatedAt,
		Ended:    t.UpdatedAt,
		Speed:    t.DownloadSpeed,
		Seeders:  t.Seeds,
		Links:    links,
		Files:    files,
	}
}

func mapStatus(tbStatus string) string {
	switch tbStatus {
	case "downloading":
		return "downloading"
	case "uploading", "uploading (seeding)", "completed":
		return "downloaded"
	case "cached":
		return "downloaded"
	case "paused":
		return "paused"
	case "error", "stalled (no seeds)":
		return "error"
	case "magnet_conversion", "checking_resume_data", "queued", "metaDL":
		return "magnet_conversion"
	default:
		return tbStatus
	}
}
