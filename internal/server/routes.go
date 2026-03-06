package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
	"github.com/ngenohkevin/debrid-vault-api/internal/realdebrid"
)

func (s *Server) registerRoutes(r *gin.Engine) {
	r.GET("/health", s.healthCheck)

	api := r.Group("/api")
	{
		api.GET("/status", s.getStatus)
		api.GET("/storage", s.getStorage)

		// Downloads
		api.POST("/downloads", s.startDownload)
		api.POST("/downloads/batch", s.startBatchDownload)
		api.GET("/downloads", s.listDownloads)
		api.GET("/downloads/events", s.downloadEvents)
		api.GET("/downloads/:id", s.getDownload)
		api.DELETE("/downloads/:id", s.cancelDownload)
		api.DELETE("/downloads/:id/remove", s.removeDownload)

		// Real-Debrid
		api.GET("/rd/user", s.getRDUser)
		api.GET("/rd/downloads", s.getRDDownloads)
		api.GET("/rd/torrents", s.getRDTorrents)
		api.GET("/rd/torrents/:id", s.getRDTorrentInfo)
		api.POST("/rd/cache/invalidate", s.invalidateRDCache)
		api.POST("/rd/unrestrict", s.unrestrictLink)

		// Library
		api.GET("/library", s.listLibrary)
		api.GET("/library/search", s.searchLibrary)
		api.POST("/library/move", s.moveMedia)
		api.DELETE("/library/*path", s.deleteMedia)
	}
}

func (s *Server) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "debrid-vault"})
}

func (s *Server) getStatus(c *gin.Context) {
	storage, _ := s.library.GetStorageInfo()
	user, _ := s.rdClient.GetUser()

	downloads := s.dlManager.GetDownloads()
	active := 0
	for _, d := range downloads {
		if d.Status == downloader.StatusDownloading || d.Status == downloader.StatusResolving || d.Status == downloader.StatusMoving {
			active++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"storage":         storage,
		"activeDownloads": active,
		"totalDownloads":  len(downloads),
		"rdUser":          user,
	})
}

func (s *Server) getStorage(c *gin.Context) {
	info, err := s.library.GetStorageInfo()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

func (s *Server) startDownload(c *gin.Context) {
	var req struct {
		Source   string              `json:"source" binding:"required"`
		Category downloader.Category `json:"category" binding:"required"`
		Folder   string              `json:"folder"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Category != downloader.CategoryMovies && req.Category != downloader.CategoryTVShows {
		c.JSON(http.StatusBadRequest, gin.H{"error": "category must be 'movies' or 'tv-shows'"})
		return
	}

	var item *downloader.DownloadItem
	var err error

	source := strings.TrimSpace(req.Source)
	folder := strings.TrimSpace(req.Folder)

	switch {
	case strings.HasPrefix(source, "magnet:"):
		item, err = s.dlManager.AddMagnet(source, req.Category)
	case strings.Contains(source, "real-debrid.com/d/"):
		item, err = s.dlManager.AddRDLink(source, req.Category, folder)
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		item, err = s.dlManager.AddDirectURL(source, "download", req.Category)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "source must be a magnet link, RD link, or HTTP URL"})
		return
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, item)
}

func (s *Server) startBatchDownload(c *gin.Context) {
	var req struct {
		Links     []string            `json:"links" binding:"required"`
		GroupName string              `json:"groupName" binding:"required"`
		Category  downloader.Category `json:"category" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Category != downloader.CategoryMovies && req.Category != downloader.CategoryTVShows {
		c.JSON(http.StatusBadRequest, gin.H{"error": "category must be 'movies' or 'tv-shows'"})
		return
	}

	items, err := s.dlManager.AddRDBatch(req.Links, req.GroupName, req.Category)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, items)
}

func (s *Server) listDownloads(c *gin.Context) {
	downloads := s.dlManager.GetDownloads()
	if downloads == nil {
		downloads = []downloader.DownloadItem{}
	}
	c.JSON(http.StatusOK, downloads)
}

func (s *Server) getDownload(c *gin.Context) {
	item, err := s.dlManager.GetDownload(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, item)
}

func (s *Server) cancelDownload(c *gin.Context) {
	if err := s.dlManager.CancelDownload(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "cancelled"})
}

func (s *Server) removeDownload(c *gin.Context) {
	if err := s.dlManager.RemoveDownload(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "removed"})
}

func (s *Server) downloadEvents(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")

	ch := s.dlManager.Subscribe()
	defer s.dlManager.Unsubscribe(ch)

	c.Stream(func(w io.Writer) bool {
		select {
		case event, ok := <-ch:
			if !ok {
				return false
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			return true
		case <-c.Request.Context().Done():
			return false
		}
	})
}

// Real-Debrid endpoints

func (s *Server) getRDUser(c *gin.Context) {
	user, err := s.rdClient.GetUser()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, user)
}

func (s *Server) getRDDownloads(c *gin.Context) {
	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			limit = parsed
		}
	}
	downloads, err := s.rdClient.ListDownloads(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if downloads == nil {
		downloads = []realdebrid.Download{}
	}
	c.JSON(http.StatusOK, downloads)
}

func (s *Server) getRDTorrents(c *gin.Context) {
	torrents, err := s.rdClient.ListTorrents()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, torrents)
}

func (s *Server) getRDTorrentInfo(c *gin.Context) {
	torrent, err := s.rdClient.GetTorrentInfo(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, torrent)
}

func (s *Server) invalidateRDCache(c *gin.Context) {
	s.rdClient.InvalidateCache()
	c.JSON(http.StatusOK, gin.H{"status": "cache invalidated"})
}

func (s *Server) unrestrictLink(c *gin.Context) {
	var req struct {
		Link string `json:"link" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := s.rdClient.UnrestrictLink(req.Link)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// Library endpoints

func (s *Server) listLibrary(c *gin.Context) {
	category := c.Query("category")
	files, err := s.library.ListMedia(category)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if files == nil {
		files = []media.MediaFile{}
	}
	c.JSON(http.StatusOK, files)
}

func (s *Server) searchLibrary(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
		return
	}
	files, err := s.library.SearchMedia(query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if files == nil {
		files = []media.MediaFile{}
	}
	c.JSON(http.StatusOK, files)
}

func (s *Server) moveMedia(c *gin.Context) {
	var req struct {
		Path     string `json:"path" binding:"required"`
		Category string `json:"category" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	newPath, err := s.library.MoveMedia(req.Path, req.Category)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "moved", "newPath": newPath})
}

func (s *Server) deleteMedia(c *gin.Context) {
	path := c.Param("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}
	if err := s.library.DeleteMedia(path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// Ensure imports are used
var (
	_ realdebrid.Download
	_ media.MediaFile
)
