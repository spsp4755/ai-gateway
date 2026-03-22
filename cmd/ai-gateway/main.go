package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/spsp4755/ai-gateway/internal/app"
	"github.com/spsp4755/ai-gateway/internal/config"
	"github.com/spsp4755/ai-gateway/internal/storage"
	"github.com/spsp4755/ai-gateway/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	store, err := storage.New(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	if err := store.Initialize(); err != nil {
		log.Fatalf("initialize storage: %v", err)
	}

	service := app.NewService(cfg, store)
	server := web.NewServer(cfg, store, service)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go service.StartHealthMonitor(rootCtx)

	httpServer := &http.Server{
		Addr:         cfg.BindAddr,
		Handler:      server.Handler(),
		ReadTimeout:  cfg.RequestTimeout,
		WriteTimeout: 0,
		IdleTimeout:  cfg.RequestTimeout,
	}

	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("ai-gateway listening on %s", cfg.BindAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

