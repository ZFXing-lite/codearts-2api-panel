package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"sync"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

var (
	assetOnce sync.Once
	assetVer  string
)

// AssetVersion is a content hash of embedded static files (for cache busting).
func AssetVersion() string {
	assetOnce.Do(func() {
		h := sha256.New()
		_ = fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := staticFiles.ReadFile(path)
			if err != nil {
				return nil
			}
			_, _ = h.Write([]byte(path))
			_, _ = h.Write(b)
			return nil
		})
		sum := hex.EncodeToString(h.Sum(nil))
		if len(sum) > 12 {
			sum = sum[:12]
		}
		if sum == "" {
			sum = strconv.FormatInt(time.Now().Unix(), 36)
		}
		assetVer = sum
	})
	return assetVer
}

func IndexHTML() []byte {
	b, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		return []byte("<h1>webui missing</h1>")
	}
	return b
}

func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Panel assets must not stick in CDN/browser after deploys.
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.StripPrefix("/static/", files).ServeHTTP(w, r)
	})
}

// CopyFile is a small helper for tests / tooling.
func CopyFile(name string, w io.Writer) error {
	b, err := staticFiles.ReadFile(name)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
