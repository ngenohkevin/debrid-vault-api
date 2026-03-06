package realdebrid

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const baseURL = "https://api.real-debrid.com/rest/1.0"

type cacheEntry[T any] struct {
	data      T
	expiresAt time.Time
}

type Client struct {
	apiKey     string
	httpClient *http.Client

	mu            sync.RWMutex
	torrentsCache *cacheEntry[[]Torrent]
	infoCache     map[string]*cacheEntry[Torrent]
	cacheTTL      time.Duration
}

func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		infoCache: make(map[string]*cacheEntry[Torrent]),
		cacheTTL:  5 * time.Minute,
	}
}

func (c *Client) InvalidateCache() {
	c.mu.Lock()
	c.torrentsCache = nil
	c.infoCache = make(map[string]*cacheEntry[Torrent])
	c.mu.Unlock()
}

func (c *Client) InvalidateTorrentInfo(id string) {
	c.mu.Lock()
	delete(c.infoCache, id)
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
		var apiErr APIError
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error != "" {
			return nil, fmt.Errorf("RD API error %d: %s", apiErr.ErrorCode, apiErr.Error)
		}
		return nil, fmt.Errorf("RD API HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}

func (c *Client) get(path string) ([]byte, error) {
	return c.doRequest("GET", path, nil, "")
}

func (c *Client) post(path string, form url.Values) ([]byte, error) {
	return c.doRequest("POST", path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
}

func (c *Client) delete(path string) ([]byte, error) {
	return c.doRequest("DELETE", path, nil, "")
}

func (c *Client) GetUser() (*User, error) {
	data, err := c.get("/user")
	if err != nil {
		return nil, err
	}
	var user User
	return &user, json.Unmarshal(data, &user)
}

func (c *Client) ListTorrents() ([]Torrent, error) {
	c.mu.RLock()
	if c.torrentsCache != nil && time.Now().Before(c.torrentsCache.expiresAt) {
		result := c.torrentsCache.data
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	data, err := c.get("/torrents")
	if err != nil {
		return nil, err
	}
	var torrents []Torrent
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.torrentsCache = &cacheEntry[[]Torrent]{data: torrents, expiresAt: time.Now().Add(c.cacheTTL)}
	c.mu.Unlock()

	return torrents, nil
}

func (c *Client) GetTorrentInfo(id string) (*Torrent, error) {
	c.mu.RLock()
	if entry, ok := c.infoCache[id]; ok && time.Now().Before(entry.expiresAt) {
		result := entry.data
		c.mu.RUnlock()
		return &result, nil
	}
	c.mu.RUnlock()

	data, err := c.get("/torrents/info/" + id)
	if err != nil {
		return nil, err
	}
	var torrent Torrent
	if err := json.Unmarshal(data, &torrent); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.infoCache[id] = &cacheEntry[Torrent]{data: torrent, expiresAt: time.Now().Add(c.cacheTTL)}
	c.mu.Unlock()

	return &torrent, nil
}

func (c *Client) AddMagnet(magnet string) (*AddMagnetResponse, error) {
	data, err := c.post("/torrents/addMagnet", url.Values{"magnet": {magnet}})
	if err != nil {
		return nil, err
	}
	var resp AddMagnetResponse
	return &resp, json.Unmarshal(data, &resp)
}

func (c *Client) SelectFiles(torrentID string, files string) error {
	_, err := c.post("/torrents/selectFiles/"+torrentID, url.Values{"files": {files}})
	return err
}

func (c *Client) DeleteTorrent(id string) error {
	_, err := c.delete("/torrents/delete/" + id)
	return err
}

func (c *Client) UnrestrictLink(link string) (*UnrestrictedLink, error) {
	data, err := c.post("/unrestrict/link", url.Values{"link": {link}})
	if err != nil {
		return nil, err
	}
	var result UnrestrictedLink
	return &result, json.Unmarshal(data, &result)
}

func (c *Client) ListDownloads(limit int) ([]Download, error) {
	path := "/downloads"
	if limit > 0 {
		path = fmt.Sprintf("/downloads?limit=%d", limit)
	}
	data, err := c.get(path)
	if err != nil {
		return nil, err
	}
	var downloads []Download
	return downloads, json.Unmarshal(data, &downloads)
}

func (c *Client) DeleteDownload(id string) error {
	_, err := c.delete("/downloads/delete/" + id)
	return err
}

// DownloadFile downloads a file from a URL and writes to the provided writer,
// reporting progress via the callback.
func (c *Client) DownloadFile(downloadURL string, w io.Writer, progress func(downloaded, total int64)) error {
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return err
	}

	dlClient := &http.Client{Timeout: 0} // no timeout for downloads
	resp, err := dlClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 256*1024) // 256KB buffer

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)
			if progress != nil {
				progress(downloaded, total)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	return nil
}
