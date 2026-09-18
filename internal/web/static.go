package web

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// embeddedStatic carries the companion app into the binary, so installing
// Timeblaster is one executable plus a config file rather than a directory tree
// that can drift out of step with the daemon.
//
//go:embed all:static
var embeddedStatic embed.FS

// spaHandler serves the PWA.
//
// Unknown paths fall back to index.html so that client-side routes survive a
// refresh or a Home Screen launch, while missing assets (a stale service worker
// asking for a file that no longer exists) still return 404 rather than a
// confusing page of HTML.
//
// Files are served directly rather than through http.FileServerFS because that
// helper redirects "/index.html" to "/", which turns both the Home Screen launch
// URL and the client-route fallback into a 301 the app has to chase.
type spaHandler struct {
	fsys fs.FS
	// startedAt stands in for a modification time. Embedded files have none, and
	// a stable per-process timestamp lets conditional requests work without
	// pinning the browser to a version across a daemon restart.
	startedAt time.Time
}

func newSPAHandler(fsys fs.FS) http.Handler {
	return &spaHandler{fsys: fsys, startedAt: time.Now()}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	if _, err := fs.Stat(h.fsys, name); err != nil {
		// A request that looks like a file (it has an extension) is a genuine
		// 404; anything else is a client-side route and gets the app shell.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
	}

	h.setCacheHeaders(w, name)
	h.serveFile(w, r, name)
}

func (h *spaHandler) setCacheHeaders(w http.ResponseWriter, name string) {
	switch {
	case name == "sw.js":
		// The service worker must never be cached, or a stale one can pin the app
		// to an old version indefinitely.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Service-Worker-Allowed", "/")
	case name == "index.html" || name == "manifest.webmanifest":
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasPrefix(name, "assets/"):
		w.Header().Set("Cache-Control", "public, max-age=86400")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
}

func (h *spaHandler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := h.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if ct := contentType(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	// ServeContent gives range requests and conditional handling for free when
	// the file can seek, which both embed.FS and os.DirFS provide.
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, h.startedAt, rs)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// contentType maps the extensions the app actually uses. mime.TypeByExtension is
// consulted first, but the system's mime.types is not guaranteed to be present
// on a Lite image, and serving app.js as text/plain would break the page.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	default:
		return mime.TypeByExtension(path.Ext(name))
	}
}
