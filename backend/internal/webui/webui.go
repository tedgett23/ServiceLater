// Package webui serves the SvelteKit SPA embedded into the binary.
// The build output is written to ./dist by adapter-static (see web/vite.config.ts).
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves static assets with precompressed (br/gz) variants and falls back to index.html for SPA routes.
func Handler() http.Handler {
	root, _ := fs.Sub(dist, "dist")
	if _, err := fs.Stat(root, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "frontend not built: run `make build` (or `npm run build` in web/)", http.StatusServiceUnavailable)
		})
	}
	return &spa{root: root}
}

type spa struct{ root fs.FS }

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if st, err := fs.Stat(s.root, name); err != nil || st.IsDir() {
		// Unknown path: SPA route, unless it looks like a missing asset.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
	}

	h := w.Header()
	h.Add("Vary", "Accept-Encoding")
	if strings.HasPrefix(name, "_app/immutable/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		h.Set("Content-Type", ct)
	}

	file := name
	ae := r.Header.Get("Accept-Encoding")
	for _, enc := range []struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if strings.Contains(ae, enc.token) {
			if _, err := fs.Stat(s.root, name+enc.ext); err == nil {
				file = name + enc.ext
				h.Set("Content-Encoding", enc.token)
				break
			}
		}
	}
	f, err := s.root.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	if rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}); ok {
		http.ServeContent(w, r, name, st.ModTime(), rs)
		return
	}
	http.NotFound(w, r)
}
