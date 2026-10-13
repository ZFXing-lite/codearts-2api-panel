package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go-codearts/internal/api"
	"go-codearts/internal/auth"
	"go-codearts/internal/config"
	"go-codearts/internal/proxypool"
	"go-codearts/internal/store"
)

func main() {
	cfg := config.Get()
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store open failed: %v", err)
	}
	secretPath := filepath.Join(cfg.DataDir, "session.secret")
	authMgr, err := auth.NewWithSecretFile(cfg.AdminUser, cfg.AdminPassword, secretPath)
	if err != nil {
		log.Fatalf("auth init failed: %v", err)
	}
	proxies, err := proxypool.OpenPool(cfg.DataDir)
	if err != nil {
		log.Fatalf("proxy pool open failed: %v", err)
	}
	apiSrv := api.New(cfg, db, authMgr, proxies)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiSrv.StartQuotaSync(ctx)

	// Live-reload .env + accounts.json without restart (task #7).
	// envPath="" falls back to "./.env" in config.ReloadEnv.
	envPath := os.Getenv("CODEARTS_ENV_FILE")
	if envPath == "" {
		envPath = ".env"
	}
	w := api.InstallHotReload(ctx, apiSrv, db, cfg.DataDir, envPath)
	defer w.Stop()

	srv := &http.Server{
		Addr:              "0.0.0.0:" + cfg.Port,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("codearts web starting on http://0.0.0.0:%s", cfg.Port)
		if cfg.PublicURL != "" {
			log.Printf("public url: %s", cfg.PublicURL)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	fmt.Println("shutting down...")
	cancel()
	// Graceful shutdown: 120s matches proxy_read_timeout headroom and keeps
	// long-thinking SSE streams alive (~80-90s observed) through drain.
	shCtx, shCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
	_ = db.Save()
	fmt.Println("shutdown complete")
}
