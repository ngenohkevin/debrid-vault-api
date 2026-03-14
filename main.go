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

	var provider debrid.Provider
	switch cfg.DebridProvider {
	case "torbox":
		if cfg.TBApiKey == "" {
			log.Fatal("TB_API_KEY required when DEBRID_PROVIDER=torbox")
		}
		provider = torbox.NewClient(cfg.TBApiKey)
	default:
		if cfg.RDApiKey == "" {
			log.Fatal("RD_API_KEY required when DEBRID_PROVIDER=realdebrid")
		}
		provider = realdebrid.NewClient(cfg.RDApiKey)
	}
	log.Printf("Using debrid provider: %s", provider.Name())

	dlManager := downloader.NewManager(cfg, provider)
	scheduler := downloader.NewScheduler(dlManager)
	library := media.NewLibrary(cfg)

	// Start stale file cleanup
	cleanupStop := make(chan struct{})
	dlManager.StartCleanup(cleanupStop)

	srv := server.New(cfg, provider, dlManager, scheduler, library)

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
