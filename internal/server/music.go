package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ngenohkevin/debrid-vault-api/internal/dab"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
)

// --- Auth (DAB fallback) ---

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
		"authenticated": true, // Tidal is always available via hifi-api
		"provider":      "tidal",
	})
}

// --- Search ---

func (s *Server) musicSearch(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
		return
	}
	searchType := c.DefaultQuery("type", "track")

	// Try Tidal first
	if s.tidal != nil {
		result, err := s.tidal.Search(query, searchType, 25)
		if err == nil {
			c.JSON(http.StatusOK, result)
			return
		}
		log.Printf("Tidal search failed, trying DAB: %v", err)
	}

	// Fallback to DAB
	s.dabSearch(c, query, searchType)
}

func (s *Server) dabSearch(c *gin.Context, query, searchType string) {
	apiType := searchType
	limit := 20
	if searchType == "album" || searchType == "artist" {
		apiType = "track"
		limit = 50
	}

	result, err := s.dab.Search(query, apiType, limit)
	if err != nil {
		if strings.Contains(err.Error(), "401") {
			if s.cfg.DABEmail != "" {
				_ = s.dab.Login(s.cfg.DABEmail, s.cfg.DABPassword)
				result, err = s.dab.Search(query, apiType, limit)
			}
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	// Extract albums/artists from track results (DAB workaround)
	if searchType == "album" && len(result.Albums) == 0 && len(result.Tracks) > 0 {
		seen := make(map[string]bool)
		for _, t := range result.Tracks {
			if t.AlbumID == "" || seen[t.AlbumID] {
				continue
			}
			seen[t.AlbumID] = true
			result.Albums = append(result.Albums, dab.Album{
				ID: t.AlbumID, Title: t.AlbumTitle, Artist: t.Artist,
				Cover: dab.CoverURL(t.AlbumCover), ReleaseDate: t.ReleaseDate, Genre: t.Genre,
			})
		}
		result.Tracks = nil
	}
	if searchType == "artist" && len(result.Artists) == 0 && len(result.Tracks) > 0 {
		seen := make(map[string]bool)
		for _, t := range result.Tracks {
			id := t.ArtistID.String()
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			result.Artists = append(result.Artists, dab.Artist{ID: t.ArtistID, Name: t.Artist})
		}
		result.Tracks = nil
	}

	c.JSON(http.StatusOK, result)
}

// --- Album ---

func (s *Server) musicAlbum(c *gin.Context) {
	albumID := c.Query("id")
	if albumID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'id' is required"})
		return
	}

	// Try Tidal
	if s.tidal != nil {
		album, err := s.tidal.GetAlbum(albumID)
		if err == nil {
			c.JSON(http.StatusOK, album)
			return
		}
		log.Printf("Tidal album failed: %v", err)
	}

	// Fallback DAB
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

// --- Artist ---

func (s *Server) musicArtist(c *gin.Context) {
	artistID := c.Query("id")
	if artistID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'id' is required"})
		return
	}

	// Try Tidal
	if s.tidal != nil {
		disco, err := s.tidal.GetArtist(artistID)
		if err == nil {
			c.JSON(http.StatusOK, disco)
			return
		}
		log.Printf("Tidal artist failed: %v", err)
	}

	// Fallback DAB
	disco, err := s.dab.GetDiscography(artistID, 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, disco)
}

// --- Download Track ---

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

	trackNum := req.TrackNumber
	if trackNum == 0 {
		trackNum = 1
	}
	filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", trackNum, req.Title, ".flac"))
	folder := req.Folder
	if folder == "" && req.Artist != "" {
		album := req.Album
		if album == "" {
			album = "Singles"
		}
		folder = sanitizeFilename(req.Artist) + "/" + sanitizeFilename(album)
	}

	// Try Tidal first (downloads via DASH/direct to local FLAC)
	if s.tidal != nil {
		item, err := s.tidalDownloadTrack(req.TrackID, filename, folder, "", "", req)
		if err == nil {
			c.JSON(http.StatusOK, item)
			return
		}
		log.Printf("Tidal track download failed, trying DAB: %v", err)
	}

	// Fallback to DAB
	quality := req.Quality
	if quality == "" {
		quality = dab.QualityFLAC
	}
	streamURL, err := s.dab.GetStreamURL(req.TrackID, quality)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to get stream URL: %v", err)})
		return
	}
	item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder, "", "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.dlManager.SetMeta(item.ID, map[string]string{
		"title": req.Title, "artist": req.Artist, "album": req.Album,
		"trackNumber": fmt.Sprintf("%d", trackNum),
	})
	c.JSON(http.StatusOK, item)
}

func (s *Server) tidalDownloadTrack(trackID, filename, folder, groupID, groupName string, req struct {
	TrackID     string `json:"trackId" binding:"required"`
	Title       string `json:"title" binding:"required"`
	Artist      string `json:"artist" binding:"required"`
	Album       string `json:"album"`
	TrackNumber int    `json:"trackNumber"`
	Quality     string `json:"quality"`
	Folder      string `json:"folder"`
}) (*downloader.DownloadItem, error) {

	// Get track info for metadata
	trackInfo, err := s.tidal.GetTrackInfo(trackID)
	if err != nil {
		return nil, fmt.Errorf("track info: %w", err)
	}

	// Create download item immediately so frontend can track it
	item := &downloader.DownloadItem{
		ID:        uuid.New().String()[:8],
		Name:      filename,
		Category:  downloader.CategoryMusic,
		Status:    downloader.StatusResolving,
		Source:    fmt.Sprintf("tidal://track/%s", trackID),
		Provider:  "tidal",
		Folder:    folder,
		GroupID:   groupID,
		GroupName: groupName,
		CreatedAt: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.dlManager.AddTrackedDownload(item, cancel)

	// Download async
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Tidal download panic for %s: %v", filename, r)
			}
		}()

		// Download FLAC via DASH/direct
		stagingDir := s.cfg.DownloadDir
		destPath := filepath.Join(stagingDir, filename)
		os.MkdirAll(filepath.Dir(destPath), 0755)

		s.dlManager.UpdateItemStatus(item.ID, downloader.StatusDownloading)

		quality, bitDepth, sampleRate, err := s.tidal.DownloadTrackAudioWithProgress(ctx, trackID, destPath, func(downloaded, total int64) {
			s.dlManager.UpdateItemProgress(item.ID, downloaded, total)
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.dlManager.SetItemError(item.ID, fmt.Sprintf("Download failed: %v", err))
			return
		}

		// Update with actual file size
		if fi, statErr := os.Stat(destPath); statErr == nil {
			s.dlManager.UpdateItemProgress(item.ID, fi.Size(), fi.Size())
		}

		// Get lyrics (best effort)
		var lyrics, syncedLyrics string
		if lyr, lyrErr := s.tidal.GetLyrics(trackID); lyrErr == nil && lyr != nil {
			lyrics = lyr.Lyrics
			syncedLyrics = lyr.SyncedLyrics
		}

		// Store metadata for post-move tagging
		trackNum := req.TrackNumber
		if trackNum == 0 {
			trackNum = trackInfo.TrackNumber
		}
		if trackNum == 0 {
			trackNum = 1
		}

		year := ""
		if trackInfo.StreamStartDate != "" && len(trackInfo.StreamStartDate) >= 4 {
			year = trackInfo.StreamStartDate[:4]
		}

		s.dlManager.SetMeta(item.ID, map[string]string{
			"title":        req.Title,
			"artist":       req.Artist,
			"album":        req.Album,
			"albumArtist":  req.Artist,
			"trackNumber":  fmt.Sprintf("%d", trackNum),
			"discNumber":   fmt.Sprintf("%d", trackInfo.VolumeNumber),
			"genre":        "",
			"year":         year,
			"cover":        coverURLFromAlbum(trackInfo.Album.Cover),
			"isrc":         trackInfo.ISRC,
			"copyright":    trackInfo.Copyright,
			"lyrics":       lyrics,
			"syncedLyrics": syncedLyrics,
			"bitDepth":     fmt.Sprintf("%d", bitDepth),
			"sampleRate":   fmt.Sprintf("%d", sampleRate),
		})

		_ = quality // logged by the tidal client

		// Hand off to download manager's move logic
		s.dlManager.MoveCompletedFile(ctx, item, destPath)
	}()

	return item, nil
}

// --- Download Album ---

func (s *Server) musicDownloadAlbum(c *gin.Context) {
	var req struct {
		AlbumID string `json:"albumId" binding:"required"`
		Quality string `json:"quality"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Try Tidal
	if s.tidal != nil {
		album, err := s.tidal.GetAlbum(req.AlbumID)
		if err == nil {
			items := s.tidalDownloadAlbum(album)
			if len(items) > 0 {
				c.JSON(http.StatusOK, gin.H{
					"album":       album.Title,
					"artist":      album.Artist,
					"tracks":      len(items),
					"totalTracks": album.TotalTracks,
					"downloads":   items,
				})
				return
			}
		}
		log.Printf("Tidal album download failed: %v", err)
	}

	// Fallback to DAB
	s.dabDownloadAlbum(c, req.AlbumID, req.Quality)
}

func (s *Server) tidalDownloadAlbum(album *dab.Album) []*downloader.DownloadItem {
	folder := sanitizeFilename(album.Artist) + "/" + sanitizeFilename(album.Title)
	groupID := uuid.New().String()[:8]
	groupName := album.Artist + " - " + album.Title

	var items []*downloader.DownloadItem
	for i, track := range album.Tracks {
		trackNum := i + 1
		filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", trackNum, track.Title, ".flac"))

		item := &downloader.DownloadItem{
			ID:        uuid.New().String()[:8],
			Name:      filename,
			Category:  downloader.CategoryMusic,
			Status:    downloader.StatusResolving,
			Source:    fmt.Sprintf("tidal://track/%s", track.ID),
			Provider:  "tidal",
			Folder:    folder,
			GroupID:   groupID,
			GroupName: groupName,
			CreatedAt: time.Now(),
		}

		ctx, cancel := context.WithCancel(context.Background())
		s.dlManager.AddTrackedDownload(item, cancel)

		// Capture loop variables
		trackID := track.ID.String()
		trackTitle := track.Title
		trackArtist := track.Artist
		if trackArtist == "" {
			trackArtist = album.Artist
		}

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Tidal download panic for %s: %v", filename, r)
				}
			}()

			// Wait for concurrency slot
			s.dlManager.AcquireSlot(ctx)
			defer s.dlManager.ReleaseSlot()

			stagingDir := s.cfg.DownloadDir
			destPath := filepath.Join(stagingDir, filename)
			os.MkdirAll(filepath.Dir(destPath), 0755)

			s.dlManager.UpdateItemStatus(item.ID, downloader.StatusDownloading)

			quality, bitDepth, sampleRate, err := s.tidal.DownloadTrackAudioWithProgress(ctx, trackID, destPath, func(downloaded, total int64) {
				s.dlManager.UpdateItemProgress(item.ID, downloaded, total)
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.dlManager.SetItemError(item.ID, fmt.Sprintf("Download failed: %v", err))
				return
			}
			_ = quality

			// Update with actual file size
			if fi, statErr := os.Stat(destPath); statErr == nil {
				s.dlManager.UpdateItemProgress(item.ID, fi.Size(), fi.Size())
			}

			// Get lyrics
			var lyrics, syncedLyrics string
			if lyr, lyrErr := s.tidal.GetLyrics(trackID); lyrErr == nil && lyr != nil {
				lyrics = lyr.Lyrics
				syncedLyrics = lyr.SyncedLyrics
			}

			year := album.Year
			if year == "" && len(album.ReleaseDate) >= 4 {
				year = album.ReleaseDate[:4]
			}

			s.dlManager.SetMeta(item.ID, map[string]string{
				"title":        trackTitle,
				"artist":       trackArtist,
				"album":        album.Title,
				"albumArtist":  album.Artist,
				"trackNumber":  fmt.Sprintf("%d", trackNum),
				"totalTracks":  fmt.Sprintf("%d", album.TotalTracks),
				"genre":        album.Genre,
				"year":         year,
				"cover":        album.Cover,
				"copyright":    album.Copyright,
				"lyrics":       lyrics,
				"syncedLyrics": syncedLyrics,
				"bitDepth":     fmt.Sprintf("%d", bitDepth),
				"sampleRate":   fmt.Sprintf("%d", sampleRate),
			})

			s.dlManager.MoveCompletedFile(ctx, item, destPath)
		}()

		items = append(items, item)
	}

	return items
}

func (s *Server) dabDownloadAlbum(c *gin.Context, albumID, quality string) {
	if quality == "" {
		quality = dab.QualityFLAC
	}
	album, err := s.dab.GetAlbum(albumID)
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

	var items []*downloader.DownloadItem
	for _, track := range album.Tracks {
		streamURL, err := s.dab.GetStreamURL(track.ID.String(), quality)
		if err != nil {
			continue
		}
		idx := trackIndex(album, track.ID)
		filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", idx, track.Title, ext))
		item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder, groupID, groupName)
		if err != nil {
			continue
		}
		year := album.ReleaseDate
		if len(year) >= 4 {
			year = year[:4]
		}
		s.dlManager.SetMeta(item.ID, map[string]string{
			"title": track.Title, "artist": track.Artist, "album": album.Title,
			"albumArtist": album.Artist, "trackNumber": fmt.Sprintf("%d", idx),
			"totalTracks": fmt.Sprintf("%d", len(album.Tracks)), "genre": album.Genre,
			"year": year, "cover": dab.CoverURL(album.Cover),
		})
		items = append(items, item)
	}

	if len(items) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to queue any tracks"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"album": album.Title, "artist": album.Artist,
		"tracks": len(items), "totalTracks": len(album.Tracks), "downloads": items,
	})
}

// --- Scheduling ---

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

	source := fmt.Sprintf("tidal://track/%s?title=%s&artist=%s&album=%s&trackNumber=%d",
		req.TrackID, url.QueryEscape(req.Title), url.QueryEscape(req.Artist),
		url.QueryEscape(req.Album), req.TrackNumber)
	name := fmt.Sprintf("%s - %s", req.Artist, req.Title)
	sched := s.scheduler.AddSchedule(name, source, downloader.CategoryMusic, "", "tidal", scheduledAt, req.SpeedLimitMbps)
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

	source := fmt.Sprintf("tidal://album/%s", req.AlbumID)
	name := req.Title
	if req.Artist != "" && req.Title != "" {
		name = req.Artist + " - " + req.Title
	}
	if name == "" {
		name = "Scheduled Album"
	}
	sched := s.scheduler.AddSchedule(name, source, downloader.CategoryMusic, "", "tidal", scheduledAt, req.SpeedLimitMbps)
	c.JSON(http.StatusOK, sched)
}

// HandleMusicSchedule processes tidal:// and dab:// sources from the scheduler.
func (s *Server) HandleMusicSchedule(source string) (*downloader.DownloadItem, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("invalid music source: %w", err)
	}

	// Route tidal:// sources
	if parsed.Scheme == "tidal" {
		return s.handleTidalSchedule(parsed)
	}

	// Route dab:// sources (backward compat)
	if parsed.Scheme == "dab" {
		return s.handleDABSchedule(parsed)
	}

	return nil, fmt.Errorf("unknown music source scheme: %s", parsed.Scheme)
}

func (s *Server) handleTidalSchedule(parsed *url.URL) (*downloader.DownloadItem, error) {
	switch parsed.Host {
	case "track":
		trackID := strings.TrimPrefix(parsed.Path, "/")
		q := parsed.Query()
		req := struct {
			TrackID     string `json:"trackId" binding:"required"`
			Title       string `json:"title" binding:"required"`
			Artist      string `json:"artist" binding:"required"`
			Album       string `json:"album"`
			TrackNumber int    `json:"trackNumber"`
			Quality     string `json:"quality"`
			Folder      string `json:"folder"`
		}{
			TrackID: trackID,
			Title:   q.Get("title"),
			Artist:  q.Get("artist"),
			Album:   q.Get("album"),
		}
		fmt.Sscanf(q.Get("trackNumber"), "%d", &req.TrackNumber)
		if req.TrackNumber == 0 {
			req.TrackNumber = 1
		}

		filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", req.TrackNumber, req.Title, ".flac"))
		folder := ""
		if req.Artist != "" {
			album := req.Album
			if album == "" {
				album = "Singles"
			}
			folder = sanitizeFilename(req.Artist) + "/" + sanitizeFilename(album)
		}

		return s.tidalDownloadTrack(trackID, filename, folder, "", "", req)

	case "album":
		albumID := strings.TrimPrefix(parsed.Path, "/")
		album, err := s.tidal.GetAlbum(albumID)
		if err != nil {
			return nil, fmt.Errorf("failed to get album: %w", err)
		}
		items := s.tidalDownloadAlbum(album)
		if len(items) == 0 {
			return nil, fmt.Errorf("failed to queue any tracks")
		}
		return items[0], nil

	default:
		return nil, fmt.Errorf("unknown tidal source type: %s", parsed.Host)
	}
}

func (s *Server) handleDABSchedule(parsed *url.URL) (*downloader.DownloadItem, error) {
	switch parsed.Host {
	case "track":
		trackID := strings.TrimPrefix(parsed.Path, "/")
		q := parsed.Query()
		title := q.Get("title")
		artist := q.Get("artist")
		album := q.Get("album")
		trackNumber := 1
		fmt.Sscanf(q.Get("trackNumber"), "%d", &trackNumber)

		streamURL, err := s.dab.GetStreamURL(trackID, dab.QualityFLAC)
		if err != nil {
			return nil, fmt.Errorf("failed to get stream URL: %w", err)
		}
		filename := sanitizeFilename(fmt.Sprintf("%02d. %s - %s%s", trackNumber, artist, title, ".flac"))
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

// --- Lyrics ---

func (s *Server) musicLyrics(c *gin.Context) {
	// Support both track ID (Tidal) and title/artist (DAB)
	trackID := c.Query("id")
	if trackID != "" && s.tidal != nil {
		lyrics, err := s.tidal.GetLyrics(trackID)
		if err == nil && lyrics != nil {
			c.JSON(http.StatusOK, lyrics)
			return
		}
	}

	title := c.Query("title")
	artist := c.Query("artist")
	if title == "" || artist == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameters 'id' or 'title'+'artist' required"})
		return
	}
	lyrics, err := s.dab.GetLyrics(title, artist)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, lyrics)
}

// --- Helpers ---

func coverURLFromAlbum(coverID string) string {
	if coverID == "" {
		return ""
	}
	path := strings.ReplaceAll(coverID, "-", "/")
	return fmt.Sprintf("https://resources.tidal.com/images/%s/1280x1280.jpg", path)
}

func trackIndex(album *dab.Album, id dab.TrackID) int {
	for i, t := range album.Tracks {
		if t.ID == id {
			return i + 1
		}
	}
	return 0
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", " -", "*", "", "?", "", "\"", "", "<", "", ">", "", "|", "",
	)
	name = replacer.Replace(name)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "unknown"
	}
	return name
}
