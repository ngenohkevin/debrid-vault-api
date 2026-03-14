package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/ngenohkevin/debrid-vault-api/internal/config"
	"github.com/ngenohkevin/debrid-vault-api/internal/debrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
	"github.com/ngenohkevin/debrid-vault-api/internal/realdebrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/server"
	"github.com/ngenohkevin/debrid-vault-api/internal/torbox"
)

func main() {
	_ = godotenv.Load()

	cfg := config.Load()

	providers := make(map[string]debrid.Provider)
	if cfg.RDApiKey != "" {
		providers["realdebrid"] = realdebrid.NewClient(cfg.RDApiKey)
	}
	if cfg.TBApiKey != "" {
		providers["torbox"] = torbox.NewClient(cfg.TBApiKey)
	}
	if len(providers) == 0 {
		log.Fatal("At least one debrid API key required (RD_API_KEY or TB_API_KEY)")
	}
	for name := range providers {
		log.Printf("Debrid provider enabled: %s", name)
	}

	dlManager := downloader.NewManager(cfg, providers)
	scheduler := downloader.NewScheduler(dlManager)
	library := media.NewLibrary(cfg)

	// Start stale file cleanup
	cleanupStop := make(chan struct{})
	dlManager.StartCleanup(cleanupStop)

	srv := server.New(cfg, providers, dlManager, scheduler, library)

	httpServer := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      srv.Router(),
		WriteTimeout: 24 * time.Hour,
		ReadTimeout:  15 * time.Second,
	}

	go func() {
		log.Printf("Debrid Vault API starting on port %s", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("Graceful shutdown timed out, forcing: %v", err)
		httpServer.Close()
	}
	close(cleanupStop)
	scheduler.Stop()
	dlManager.Shutdown()
	log.Println("Server stopped")
}
