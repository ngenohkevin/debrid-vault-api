package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
	// Always search as "track" and extract albums/artists from results.
	apiType := searchType
	if searchType == "album" || searchType == "artist" {
		apiType = "track"
	}

	result, err := s.dab.Search(query, apiType, 50)
	if err != nil {
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "requiresAuth") {
			if s.dabRelogin() {
				result, err = s.dab.Search(query, apiType, 50)
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
				ID:     t.AlbumID,
				Title:  t.AlbumTitle,
				Artist: t.Artist,
				Cover:  dab.CoverURL(t.AlbumCover),
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

	item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder)
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

	// Resolve all stream URLs and queue downloads
	var items []*downloader.DownloadItem
	for _, track := range album.Tracks {
		streamURL, err := s.dab.GetStreamURL(track.ID.String(), quality)
		if err != nil {
			continue // skip tracks that fail
		}

		idx := trackIndex(album, track.ID)
		filename := sanitizeFilename(fmt.Sprintf("%02d. %s%s", idx, track.Title, ext))

		item, err := s.dlManager.AddMusicDownload(streamURL, filename, folder)
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
