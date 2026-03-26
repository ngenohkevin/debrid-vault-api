package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ngenohkevin/debrid-vault-api/internal/debrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
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
		api.GET("/downloads/completed-sources", s.getCompletedSources)
		api.GET("/downloads/events", s.downloadEvents)
		api.GET("/downloads/:id", s.getDownload)
		api.DELETE("/downloads/:id", s.cancelDownload)
		api.DELETE("/downloads/:id/remove", s.removeDownload)
		api.POST("/downloads/:id/pause", s.pauseDownload)
		api.POST("/downloads/:id/resume", s.resumeDownload)
		api.POST("/downloads/:id/retry-move", s.retryMove)

		// Schedules
		api.GET("/schedules", s.listSchedules)
		api.POST("/schedules", s.createSchedule)
		api.GET("/schedules/:id", s.getSchedule)
		api.PUT("/schedules/:id", s.updateSchedule)
		api.DELETE("/schedules/:id", s.cancelSchedule)
		api.DELETE("/schedules/:id/remove", s.removeSchedule)
		api.GET("/downloads/:id/resumable", s.checkResumable)
		api.POST("/downloads/:id/schedule", s.scheduleExisting)
		api.POST("/downloads/group/:groupId/schedule", s.scheduleExistingGroup)

		// Settings
		api.GET("/settings", s.getSettings)
		api.PUT("/settings", s.updateSettings)

		// Providers
		api.GET("/providers", s.listProviders)

		// Real-Debrid / Cloud
		api.GET("/rd/user", s.getRDUser)
		api.GET("/rd/downloads", s.getRDDownloads)
		api.GET("/rd/torrents", s.getRDTorrents)
		api.GET("/rd/torrents/:id", s.getRDTorrentInfo)
		api.POST("/rd/cache/invalidate", s.invalidateRDCache)
		api.POST("/rd/unrestrict", s.unrestrictLink)

		// Music (DAB)
		api.GET("/music/search", s.musicSearch)
		api.GET("/music/album", s.musicAlbum)
		api.GET("/music/artist", s.musicArtist)
		api.POST("/music/download/track", s.musicDownloadTrack)
		api.POST("/music/download/album", s.musicDownloadAlbum)
		api.GET("/music/lyrics", s.musicLyrics)
		api.POST("/music/upload", s.musicUpload)
		api.POST("/music/schedule/track", s.musicScheduleTrack)
		api.POST("/music/schedule/album", s.musicScheduleAlbum)
		api.POST("/music/login", s.musicLogin)
		api.GET("/music/status", s.musicStatus)

		// Library
		api.GET("/library", s.listLibrary)
		api.GET("/library/search", s.searchLibrary)
		api.POST("/library/move", s.moveMedia)
		api.DELETE("/library/*path", s.deleteMedia)
		api.GET("/library/subtitles", s.probeSubtitles)
	}
}

func (s *Server) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "debrid-vault", "providers": s.providerNames()})
}

func (s *Server) listProviders(c *gin.Context) {
	type providerInfo struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	providers := make([]providerInfo, 0, len(s.providers))
	for name, p := range s.providers {
		providers = append(providers, providerInfo{Name: name, DisplayName: p.Name()})
	}
	c.JSON(http.StatusOK, providers)
}

func (s *Server) getStatus(c *gin.Context) {
	storage, _ := s.library.GetStorageInfo()

	downloads := s.dlManager.GetDownloads()
	active := 0
	for _, d := range downloads {
		if d.Status == downloader.StatusDownloading || d.Status == downloader.StatusResolving || d.Status == downloader.StatusMoving || d.Status == downloader.StatusQueued {
			active++
		}
	}

	users := make(map[string]interface{})
	for name, p := range s.providers {
		user, err := p.GetUser()
		if err == nil {
			users[name] = user
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"storage":         storage,
		"activeDownloads": active,
		"totalDownloads":  len(downloads),
		"users":           users,
		"providers":       s.providerNames(),
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
		Provider string              `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Category != downloader.CategoryMovies && req.Category != downloader.CategoryTVShows && req.Category != downloader.CategoryMusic {
		c.JSON(http.StatusBadRequest, gin.H{"error": "category must be 'movies', 'tv-shows', or 'music'"})
		return
	}

	provider := req.Provider
	if provider == "" {
		provider = "realdebrid"
	}

	var item *downloader.DownloadItem
	var err error

	source := strings.TrimSpace(req.Source)
	folder := strings.TrimSpace(req.Folder)

	switch {
	case strings.HasPrefix(source, "magnet:"):
		item, err = s.dlManager.AddMagnet(source, req.Category, provider)
	case strings.Contains(source, "real-debrid.com/d/"):
		item, err = s.dlManager.AddRDLink(source, req.Category, folder, provider)
	case strings.HasPrefix(source, "tb://"):
		item, err = s.dlManager.AddRDLink(source, req.Category, folder, provider)
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		item, err = s.dlManager.AddDirectURL(source, "download", req.Category, provider)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "source must be a magnet link, RD link, TB link, or HTTP URL"})
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
		Provider  string              `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Category != downloader.CategoryMovies && req.Category != downloader.CategoryTVShows && req.Category != downloader.CategoryMusic {
		c.JSON(http.StatusBadRequest, gin.H{"error": "category must be 'movies', 'tv-shows', or 'music'"})
		return
	}

	provider := req.Provider
	if provider == "" {
		provider = "realdebrid"
	}

	items, err := s.dlManager.AddRDBatch(req.Links, req.GroupName, req.Category, provider)
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

func (s *Server) getCompletedSources(c *gin.Context) {
	sources := s.dlManager.GetCompletedSources()
	if sources == nil {
		sources = []string{}
	}
	c.JSON(http.StatusOK, sources)
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
	c.Header("Cache-Control", "no-cache, no-store, no-transform, must-revalidate")
	c.Header("X-Accel-Buffering", "no")
	c.Header("X-Content-Type-Options", "nosniff")

	ch := s.dlManager.Subscribe()
	defer s.dlManager.Unsubscribe(ch)

	// Heartbeat every 30s to keep Cloudflare from timing out (100s idle limit)
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	c.Stream(func(w io.Writer) bool {
		select {
		case event, ok := <-ch:
			if !ok {
				return false
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			return true
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			return true
		case <-c.Request.Context().Done():
			return false
		}
	})
}

// Real-Debrid endpoints

func (s *Server) getRDUser(c *gin.Context) {
	user, err := s.provider(c.DefaultQuery("provider", "realdebrid")).GetUser()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, user)
}

type rdDownloadWithSubs struct {
	debrid.Download
	SubtitleStatus downloader.SubtitleStatus `json:"subtitleStatus"`
}

func (s *Server) getRDDownloads(c *gin.Context) {
	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			limit = parsed
		}
	}
	downloads, err := s.provider(c.DefaultQuery("provider", "realdebrid")).ListDownloads(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	result := make([]rdDownloadWithSubs, len(downloads))
	for i, d := range downloads {
		result[i] = rdDownloadWithSubs{
			Download:       d,
			SubtitleStatus: downloader.DetectSubtitleStatus(d.Filename),
		}
	}
	c.JSON(http.StatusOK, result)
}

type rdTorrentWithSubs struct {
	debrid.Torrent
	SubtitleStatus downloader.SubtitleStatus `json:"subtitleStatus"`
}

func (s *Server) getRDTorrents(c *gin.Context) {
	torrents, err := s.provider(c.DefaultQuery("provider", "realdebrid")).ListTorrents()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	result := make([]rdTorrentWithSubs, len(torrents))
	for i, t := range torrents {
		result[i] = rdTorrentWithSubs{
			Torrent:        t,
			SubtitleStatus: downloader.DetectSubtitleStatus(t.Filename),
		}
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) getRDTorrentInfo(c *gin.Context) {
	torrent, err := s.provider(c.DefaultQuery("provider", "realdebrid")).GetTorrentInfo(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rdTorrentWithSubs{
		Torrent:        *torrent,
		SubtitleStatus: downloader.DetectSubtitleStatus(torrent.Filename),
	})
}

func (s *Server) invalidateRDCache(c *gin.Context) {
	s.provider(c.DefaultQuery("provider", "realdebrid")).InvalidateCache()
	c.JSON(http.StatusOK, gin.H{"status": "cache invalidated"})
}

func (s *Server) unrestrictLink(c *gin.Context) {
	var req struct {
		Link     string `json:"link" binding:"required"`
		Provider string `json:"provider"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	provider := req.Provider
	if provider == "" {
		provider = "realdebrid"
	}
	result, err := s.provider(provider).UnrestrictLink(req.Link)
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

func (s *Server) probeSubtitles(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path parameter is required"})
		return
	}

	info, err := s.library.ProbeSubtitlesForPath(path)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

func (s *Server) pauseDownload(c *gin.Context) {
	if err := s.dlManager.PauseDownload(c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "paused"})
}

func (s *Server) resumeDownload(c *gin.Context) {
	if err := s.dlManager.ResumeDownload(c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "resumed"})
}

func (s *Server) retryMove(c *gin.Context) {
	if err := s.dlManager.RetryMove(c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "retrying move"})
}

func (s *Server) getSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"maxConcurrentDownloads": s.cfg.MaxConcurrentDownloads,
		"maxSegmentsPerFile":     s.cfg.MaxSegmentsPerFile,
		"speedLimitMbps":         s.cfg.SpeedLimitMbps,
	})
}

func (s *Server) updateSettings(c *gin.Context) {
	var req struct {
		SpeedLimitMbps *float64 `json:"speedLimitMbps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.SpeedLimitMbps != nil {
		s.cfg.SpeedLimitMbps = *req.SpeedLimitMbps
		s.dlManager.Engine().SetSpeedLimit(*req.SpeedLimitMbps)
		s.cfg.SaveSettings()
	}
	c.JSON(http.StatusOK, gin.H{
		"maxConcurrentDownloads": s.cfg.MaxConcurrentDownloads,
		"maxSegmentsPerFile":     s.cfg.MaxSegmentsPerFile,
		"speedLimitMbps":         s.cfg.SpeedLimitMbps,
	})
}

// Schedule endpoints

func (s *Server) listSchedules(c *gin.Context) {
	schedules := s.scheduler.GetSchedules()
	if schedules == nil {
		schedules = []downloader.ScheduledDownload{}
	}
	c.JSON(http.StatusOK, schedules)
}

func (s *Server) createSchedule(c *gin.Context) {
	var req struct {
		Name           string              `json:"name"`
		Source         string              `json:"source" binding:"required"`
		Category       downloader.Category `json:"category" binding:"required"`
		Folder         string              `json:"folder"`
		Provider       string              `json:"provider"`
		ScheduledAt    string              `json:"scheduledAt" binding:"required"`
		SpeedLimitMbps float64             `json:"speedLimitMbps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Category != downloader.CategoryMovies && req.Category != downloader.CategoryTVShows && req.Category != downloader.CategoryMusic {
		c.JSON(http.StatusBadRequest, gin.H{"error": "category must be 'movies', 'tv-shows', or 'music'"})
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

	provider := req.Provider
	if provider == "" {
		provider = "realdebrid"
	}

	sched := s.scheduler.AddSchedule(
		strings.TrimSpace(req.Name),
		strings.TrimSpace(req.Source),
		req.Category,
		strings.TrimSpace(req.Folder),
		provider,
		scheduledAt,
		req.SpeedLimitMbps,
	)
	c.JSON(http.StatusOK, sched)
}

func (s *Server) getSchedule(c *gin.Context) {
	sched, err := s.scheduler.GetSchedule(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sched)
}

func (s *Server) updateSchedule(c *gin.Context) {
	var req struct {
		ScheduledAt    *string  `json:"scheduledAt"`
		SpeedLimitMbps *float64 `json:"speedLimitMbps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var scheduledAt *time.Time
	if req.ScheduledAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ScheduledAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "scheduledAt must be RFC3339 format"})
			return
		}
		if t.Before(time.Now()) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "scheduledAt must be in the future"})
			return
		}
		scheduledAt = &t
	}

	sched, err := s.scheduler.UpdateSchedule(c.Param("id"), scheduledAt, req.SpeedLimitMbps)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sched)
}

func (s *Server) cancelSchedule(c *gin.Context) {
	if err := s.scheduler.CancelSchedule(c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "cancelled"})
}

func (s *Server) checkResumable(c *gin.Context) {
	if err := s.dlManager.CheckResumable(c.Param("id")); err != nil {
		c.JSON(http.StatusOK, gin.H{"resumable": false, "reason": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"resumable": true})
}

func (s *Server) scheduleExisting(c *gin.Context) {
	var req struct {
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

	sched, err := s.scheduler.ScheduleExisting(c.Param("id"), scheduledAt, req.SpeedLimitMbps)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sched)
}

func (s *Server) scheduleExistingGroup(c *gin.Context) {
	var req struct {
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

	// Find any item in the group to pass to ScheduleExisting
	groupID := c.Param("groupId")
	items := s.dlManager.GetDownloadsByGroup(groupID)
	if len(items) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
		return
	}

	sched, err := s.scheduler.ScheduleExisting(items[0].ID, scheduledAt, req.SpeedLimitMbps)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sched)
}

func (s *Server) removeSchedule(c *gin.Context) {
	if err := s.scheduler.RemoveSchedule(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "removed"})
}

// Ensure import is used
var _ media.MediaFile
