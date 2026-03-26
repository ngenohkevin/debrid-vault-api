package dab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	baseURL   = "https://dabmusic.xyz/api"
	userAgent = "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	QualityFLAC = "27"
	QualityMP3  = "5"
)

type Client struct {
	http    *http.Client
	session string
}

func NewClient() *Client {
	return &Client{
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func NewClientWithSession(session string) *Client {
	c := NewClient()
	c.session = session
	return c
}

func (c *Client) SetSession(session string) {
	c.session = session
}

func (c *Client) Session() string {
	return c.session
}

// Login authenticates with DAB Music and stores the session cookie.
func (c *Client) Login(email, password string) error {
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
	req, err := http.NewRequest("POST", baseURL+"/auth/login", jsonReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid credentials")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("login failed with status %d", resp.StatusCode)
	}

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "session" {
			c.session = cookie.Value
			return nil
		}
	}
	return fmt.Errorf("no session cookie in response")
}

// Search searches for tracks, albums, or artists.
func (c *Client) Search(query, searchType string, limit int) (*SearchResult, error) {
	params := url.Values{"q": {query}}
	if searchType != "" {
		params.Set("type", searchType)
	}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}

	var result SearchResult
	if err := c.get("/search?"+params.Encode(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetTrack returns track metadata.
func (c *Client) GetTrack(trackID string) (*TrackDetail, error) {
	params := url.Values{"trackId": {trackID}}
	var result struct {
		Track TrackDetail `json:"track"`
	}
	if err := c.get("/track?"+params.Encode(), &result); err != nil {
		return nil, err
	}
	return &result.Track, nil
}

// GetStreamURL returns a direct download URL for a track.
func (c *Client) GetStreamURL(trackID, quality string) (string, error) {
	if quality == "" {
		quality = QualityFLAC
	}
	params := url.Values{"trackId": {trackID}, "quality": {quality}}
	var result map[string]interface{}
	if err := c.get("/stream?"+params.Encode(), &result); err != nil {
		return "", err
	}
	// API returns either "url" or "streamUrl"
	if u, ok := result["url"].(string); ok && u != "" {
		return u, nil
	}
	if u, ok := result["streamUrl"].(string); ok && u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no stream URL in response")
}

// GetAlbum returns album info with track list.
func (c *Client) GetAlbum(albumID string) (*Album, error) {
	params := url.Values{"albumId": {albumID}}
	var result struct {
		Album Album `json:"album"`
	}
	if err := c.get("/album?"+params.Encode(), &result); err != nil {
		return nil, err
	}
	return &result.Album, nil
}

// GetDiscography returns an artist's album list.
func (c *Client) GetDiscography(artistID string, limit int) (*DiscographyResult, error) {
	params := url.Values{"artistId": {artistID}, "sortBy": {"year"}, "sortOrder": {"desc"}}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}
	var result DiscographyResult
	if err := c.get("/discography?"+params.Encode(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetLyrics returns lyrics for a track.
func (c *Client) GetLyrics(title, artist string) (*Lyrics, error) {
	params := url.Values{"title": {title}, "artist": {artist}}
	var result Lyrics
	if err := c.get("/lyrics?"+params.Encode(), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CoverURL returns the full URL for a cover image path.
func CoverURL(cover string) string {
	if cover == "" {
		return ""
	}
	if cover[0] == '/' {
		return baseURL + cover
	}
	return cover
}

func (c *Client) get(path string, out interface{}) error {
	req, err := http.NewRequest("GET", baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	if c.session != "" {
		req.Header.Set("Cookie", "session="+c.session)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return fmt.Errorf("rate limited, try again later")
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func jsonReader(s string) io.Reader {
	return strings.NewReader(s)
}
