package server

import (
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/ngenohkevin/debrid-vault-api/internal/config"
	"github.com/ngenohkevin/debrid-vault-api/internal/debrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
)

type Server struct {
	cfg       *config.Config
	rdClient  debrid.Provider
	dlManager *downloader.Manager
	scheduler *downloader.Scheduler
	library   *media.Library
}

func New(cfg *config.Config, rdClient debrid.Provider, dlManager *downloader.Manager, scheduler *downloader.Scheduler, library *media.Library) *Server {
	return &Server{
		cfg:       cfg,
		rdClient:  rdClient,
		dlManager: dlManager,
		scheduler: scheduler,
		library:   library,
	}
}

func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	// CORS
	origins := strings.Split(s.cfg.AllowedOrigins, ",")
	r.Use(cors.New(cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-API-Key"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
	}))

	// API key middleware (optional)
	if s.cfg.APIKey != "" {
		r.Use(func(c *gin.Context) {
			// Skip health check
			if c.Request.URL.Path == "/health" {
				c.Next()
				return
			}
			key := c.GetHeader("X-API-Key")
			if key == "" {
				key = c.Query("api_key")
			}
			if key != s.cfg.APIKey {
				c.JSON(401, gin.H{"error": "unauthorized"})
				c.Abort()
				return
			}
			c.Next()
		})
	}

	s.registerRoutes(r)
	return r
}
