package api

import (
	"context"
	"log"
	"path/filepath"

	"go-codearts/internal/config"
	"go-codearts/internal/store"
	"go-codearts/internal/watcher"
)

// InstallHotReload wires the mtime-poll watcher to two live-reload paths:
//
//   - .env change → config.ReloadEnv + config.Get + srv.ApplyReloadable
//     (only the whitelisted hot subset — Rotate*, SSEIdleTimeout,
//     RespCacheTTL, HuaweiDispatchRPS, KeyRPM, KeyMaxInFlight, ForceThinking,
//     EarlyEnvelope, HeaderWaitDepleted, NonStreamTimeoutDepleted)
//   - accounts.json change → db.ReloadFromDisk + srv.SyncShardFromDB
//     (fs-swap authoritative; parse errors preserve live state)
//
// The returned Watcher can be Stop()'d from cmd/server's shutdown path.
// ctx cancellation halts the poller cooperatively. Poll cadence 2s + 500ms
// debounce matches the "operator-hot-reload" latency target (sub-3s).
//
// Wiring in cmd/server/main.go (single call):
//
//	w := api.InstallHotReload(ctx, srv, db, cfg.DataDir, ".env")
//	defer w.Stop()
func InstallHotReload(ctx context.Context, srv *Server, db *store.DB, dataDir, envPath string) *watcher.Watcher {
	accountsPath := filepath.Join(dataDir, "accounts.json")
	w := watcher.New(0, 0, func(path string) {
		switch path {
		case envPath:
			config.ReloadEnv(envPath)
			cfg := config.Get()
			srv.ApplyReloadable(cfg)
			log.Printf("hot-reload env applied: rotate=%d/%d sseIdle=%s respCacheTTL=%s huaweiRPS=%d",
				cfg.RotateAfterReady, cfg.RotateAfterDepleted, cfg.SSEIdleTimeout,
				cfg.RespCacheTTL, cfg.HuaweiDispatchRPS)
		case accountsPath:
			if err := db.ReloadFromDisk(); err != nil {
				log.Printf("hot-reload accounts.json rejected: %v (live state preserved)", err)
				return
			}
			srv.SyncShardFromDB()
			log.Printf("hot-reload accounts.json applied: %d accounts", len(db.SnapshotList()))
		}
	})
	w.Add(envPath)
	w.Add(accountsPath)
	w.Start(ctx)
	return w
}
