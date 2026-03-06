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
	"github.com/ngenohkevin/debrid-vault-api/internal/downloader"
	"github.com/ngenohkevin/debrid-vault-api/internal/media"
	"github.com/ngenohkevin/debrid-vault-api/internal/realdebrid"
	"github.com/ngenohkevin/debrid-vault-api/internal/server"
)

func main() {
	_ = godotenv.Load()

	cfg := config.Load()

	rdClient := realdebrid.NewClient(cfg.RDApiKey)
	dlManager := downloader.NewManager(cfg, rdClient)
	library := media.NewLibrary(cfg)

	srv := server.New(cfg, rdClient, dlManager, library)

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}
	dlManager.Shutdown()
	log.Println("Server stopped")
}
