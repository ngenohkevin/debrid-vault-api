package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ngenohkevin/debrid-vault-api/internal/dab"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
)

func (s *Server) musicLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.dab.Login(req.Email, req.Password); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "logged in"})
}

func (s *Server) musicStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"authenticated": s.dab.Session() != "",
	})
}

// dabRelogin attempts to re-authenticate using stored credentials.
func (s *Server) dabRelogin() bool {
	if s.cfg.DABEmail != "" && s.cfg.DABPassword != "" {
		if err := s.dab.Login(s.cfg.DABEmail, s.cfg.DABPassword); err == nil {
			return true
		}
	}
	return false
}

func (s *Server) musicSearch(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
		return
	}
	searchType := c.DefaultQuery("type", "track")

	// DAB API often returns tracks even when searching for albums/artists.
	// Search as "track" and extract albums/artists from results.
	apiType := searchType
	limit := 20
	if searchType == "album" || searchType == "artist" {
		apiType = "track"
		limit = 50
	}

	result, err := s.dab.Search(query, apiType, limit)
	if err != nil {
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "requiresAuth") {
			if s.dabRelogin() {
				result, err = s.dab.Search(query, apiType, limit)
			}
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	// Extract unique albums from track results
	if searchType == "album" && len(result.Albums) == 0 && len(result.Tracks) > 0 {
		seen := make(map[string]bool)
		for _, t := range result.Tracks {
			if t.AlbumID == "" || seen[t.AlbumID] {
				continue
			}
			seen[t.AlbumID] = true
			result.Albums = append(result.Albums, dab.Album{
				ID:          t.AlbumID,
				Title:       t.AlbumTitle,
				Artist:      t.Artist,
				Cover:       dab.CoverURL(t.AlbumCover),
				ReleaseDate: t.ReleaseDate,
				Genre:       t.Genre,
			})
		}
		result.Tracks = nil
	}

	// Extract unique artists from track results
	if searchType == "artist" && len(result.Artists) == 0 && len(result.Tracks) > 0 {
		seen := make(map[string]bool)
		for _, t := range result.Tracks {
			artistID := t.ArtistID.String()
			if artistID == "" || seen[artistID] {
				continue
			}
			seen[artistID] = true
			result.Artists = append(result.Artists, dab.Artist{
				ID:   t.ArtistID,
				Name: t.Artist,
			})
		}
		result.Tracks = nil
	}

	c.JSON(http.StatusOK, result)
}

func (s *Server) musicAlbum(c *gin.Context) {
	albumID := c.Query("id")
	if albumID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'id' is required"})
		return
	}
	album, err := s.dab.GetAlbum(albumID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	album.Cover = dab.CoverURL(album.Cover)
	for i := range album.Tracks {
		album.Tracks[i].AlbumCover = dab.CoverURL(album.Tracks[i].AlbumCover)
	}
	c.JSON(http.StatusOK, album)
}

func (s *Server) musicArtist(c *gin.Context) {
	artistID := c.Query("id")
	if artistID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'id' is required"})
		return
	}
	disco, err := s.dab.GetDiscography(artistID, 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, disco)
}

func (s *Server) musicDownloadTrack(c *gin.Context) {
	var req struct {
		TrackID     string `json:"trackId" binding:"required"`
		Title       string `json:"title" binding:"required"`
		Artist      string `json:"artist" binding:"required"`
		Album       string `json:"album"`
		TrackNumber int    `json:"trackNumber"`
		Quality     string `json:"quality"`
		Folder      string `json:"folder"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	quality := req.Quality
	if quality == "" {
		quality = dab.QualityFLAC
	}

	// Get stream URL
	streamURL, err := s.dab.GetStreamURL(req.TrackID, quality)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to get stream URL: %v", err)})
		return
	}

	// Build filename and folder
	ext := ".flac"
	if quality == dab.QualityMP3 {
		ext = ".mp3"
	}
	trackNum := req.TrackNumber
	if trackNum == 0 {
		trackNum = 1
	}
	filename := sanitizeFilename(fmt.Sprintf("%02d. %s - %s%s", trackNum, req.Artist, req.Title, ext))
	folder := req.Folder
	if folder == "" && req.Artist != "" {
		album := req.Album
		if album == "" {
			album = "Singles"
		}
		folder = sanitizeFilename(req.Artist) + "/" + sanitizeFilename(album)
	}

	item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder, "", "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, item)
}

func (s *Server) musicDownloadAlbum(c *gin.Context) {
	var req struct {
		AlbumID string `json:"albumId" binding:"required"`
		Quality string `json:"quality"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	quality := req.Quality
	if quality == "" {
		quality = dab.QualityFLAC
	}

	album, err := s.dab.GetAlbum(req.AlbumID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to get album: %v", err)})
		return
	}

	if len(album.Tracks) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "album has no tracks"})
		return
	}

	folder := sanitizeFilename(album.Artist) + "/" + sanitizeFilename(album.Title)
	ext := ".flac"
	if quality == dab.QualityMP3 {
		ext = ".mp3"
	}

	groupID := uuid.New().String()[:8]
	groupName := album.Artist + " - " + album.Title

	// Resolve all stream URLs and queue downloads
	var items []*downloader.DownloadItem
	for _, track := range album.Tracks {
		streamURL, err := s.dab.GetStreamURL(track.ID.String(), quality)
		if err != nil {
			continue // skip tracks that fail
		}

		idx := trackIndex(album, track.ID)
		filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", idx, track.Title, ext))

		item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder, groupID, groupName)
		if err != nil {
			continue
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue any tracks"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"album":       album.Title,
		"artist":      album.Artist,
		"tracks":      len(items),
		"totalTracks": len(album.Tracks),
		"downloads":   items,
	})
}

func (s *Server) musicScheduleTrack(c *gin.Context) {
	var req struct {
		TrackID        string  `json:"trackId" binding:"required"`
		Title          string  `json:"title" binding:"required"`
		Artist         string  `json:"artist" binding:"required"`
		Album          string  `json:"album"`
		TrackNumber    int     `json:"trackNumber"`
		ScheduledAt    string  `json:"scheduledAt" binding:"required"`
		SpeedLimitMbps float64 `json:"speedLimitMbps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	scheduledAt, err := time.Parse(time.RFC3339, req.ScheduledAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scheduledAt must be RFC3339 format"})
		return
	}
	if scheduledAt.Before(time.Now()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scheduledAt must be in the future"})
		return
	}

	// Encode track info in the source URL
	source := fmt.Sprintf("dab://track/%s?title=%s&artist=%s&album=%s&trackNumber=%d",
		req.TrackID,
		url.QueryEscape(req.Title),
		url.QueryEscape(req.Artist),
		url.QueryEscape(req.Album),
		req.TrackNumber,
	)
	name := fmt.Sprintf("%s - %s", req.Artist, req.Title)

	sched := s.scheduler.AddSchedule(name, source, downloader.CategoryMusic, "", "dab", scheduledAt, req.SpeedLimitMbps)
	c.JSON(http.StatusOK, sched)
}

func (s *Server) musicScheduleAlbum(c *gin.Context) {
	var req struct {
		AlbumID        string  `json:"albumId" binding:"required"`
		Title          string  `json:"title"`
		Artist         string  `json:"artist"`
		ScheduledAt    string  `json:"scheduledAt" binding:"required"`
		SpeedLimitMbps float64 `json:"speedLimitMbps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	scheduledAt, err := time.Parse(time.RFC3339, req.ScheduledAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scheduledAt must be RFC3339 format"})
		return
	}
	if scheduledAt.Before(time.Now()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scheduledAt must be in the future"})
		return
	}

	source := fmt.Sprintf("dab://album/%s", req.AlbumID)
	name := req.Title
	if req.Artist != "" && req.Title != "" {
		name = req.Artist + " - " + req.Title
	}
	if name == "" {
		name = "Scheduled Album"
	}

	sched := s.scheduler.AddSchedule(name, source, downloader.CategoryMusic, "", "dab", scheduledAt, req.SpeedLimitMbps)
	c.JSON(http.StatusOK, sched)
}

// HandleMusicSchedule processes dab:// sources from the scheduler.
// It resolves fresh stream URLs at execution time (since they expire).
func (s *Server) HandleMusicSchedule(source string) (*downloader.DownloadItem, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("invalid dab source: %w", err)
	}

	switch parsed.Host {
	case "track":
		trackID := strings.TrimPrefix(parsed.Path, "/")
		q := parsed.Query()
		title := q.Get("title")
		artist := q.Get("artist")
		album := q.Get("album")
		trackNumber := 1
		if tn := q.Get("trackNumber"); tn != "" {
			fmt.Sscanf(tn, "%d", &trackNumber)
		}

		streamURL, err := s.dab.GetStreamURL(trackID, dab.QualityFLAC)
		if err != nil {
			return nil, fmt.Errorf("failed to get stream URL: %w", err)
		}

		ext := ".flac"
		if trackNumber == 0 {
			trackNumber = 1
		}
		filename := sanitizeFilename(fmt.Sprintf("%02d. %s - %s%s", trackNumber, artist, title, ext))
		folder := ""
		if artist != "" {
			albumName := album
			if albumName == "" {
				albumName = "Singles"
			}
			folder = sanitizeFilename(artist) + "/" + sanitizeFilename(albumName)
		}

		return s.dlManager.AddMusicDownload(streamURL, filename, folder, "", "")

	case "album":
		albumID := strings.TrimPrefix(parsed.Path, "/")
		albumInfo, err := s.dab.GetAlbum(albumID)
		if err != nil {
			return nil, fmt.Errorf("failed to get album: %w", err)
		}
		if len(albumInfo.Tracks) == 0 {
			return nil, fmt.Errorf("album has no tracks")
		}

		folder := sanitizeFilename(albumInfo.Artist) + "/" + sanitizeFilename(albumInfo.Title)
		groupID := uuid.New().String()[:8]
		groupName := albumInfo.Artist + " - " + albumInfo.Title

		var firstItem *downloader.DownloadItem
		for _, track := range albumInfo.Tracks {
			streamURL, err := s.dab.GetStreamURL(track.ID.String(), dab.QualityFLAC)
			if err != nil {
				continue
			}
			idx := trackIndex(albumInfo, track.ID)
			filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", idx, track.Title, ".flac"))
			item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder, groupID, groupName)
			if err != nil {
				continue
			}
			if firstItem == nil {
				firstItem = item
			}
		}
		if firstItem == nil {
			return nil, fmt.Errorf("failed to queue any tracks")
		}
		return firstItem, nil

	default:
		return nil, fmt.Errorf("unknown dab source type: %s", parsed.Host)
	}
}

func (s *Server) musicLyrics(c *gin.Context) {
	title := c.Query("title")
	artist := c.Query("artist")
	if title == "" || artist == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameters 'title' and 'artist' are required"})
		return
	}
	lyrics, err := s.dab.GetLyrics(title, artist)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, lyrics)
}

// trackIndex returns 1-based index of a track in the album by ID.
func trackIndex(album *dab.Album, id dab.TrackID) int {
	for i, t := range album.Tracks {
		if t.ID == id {
			return i + 1
		}
	}
	return 0
}

// sanitizeFilename removes or replaces characters unsafe for filenames.
func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", " -",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	name = replacer.Replace(name)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "unknown"
	}
	return name
}
