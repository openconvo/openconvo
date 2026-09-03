package http

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPAHandler serves the embedded single-page frontend:
//
//   - real files are served as-is (hashed /assets/* immutably cached);
//   - any other path falls back to index.html so client-side routes
//     like /channels/123 work on hard refresh;
//   - when no frontend build is embedded, a plain informational page is
//     served so the binary remains useful during backend development.
func SPAHandler(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if assets == nil {
			serveFallbackPage(w)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		if info, err := fs.Stat(assets, name); err == nil && info.Mode().IsRegular() {
			if strings.HasPrefix(name, "assets/") {
				// Vite emits content-hashed filenames: safe to cache forever.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, assets, name)
			return
		}

		// Client-side route: serve the SPA shell.
		if _, err := fs.Stat(assets, "index.html"); err != nil {
			serveFallbackPage(w)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, assets, "index.html")
	})
}

const fallbackPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OpenConvo</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 40rem; margin: 15vh auto; padding: 0 1.5rem; color: #1a1f1c; }
  code { background: #eef1ef; padding: .15rem .4rem; border-radius: 4px; }
  @media (prefers-color-scheme: dark) { body { background: #101413; color: #e6eae8; } code { background: #232927; } }
</style>
<h1>OpenConvo is running</h1>
<p>The backend is up, but the frontend assets are not embedded in this build.</p>
<p>Build them with <code>make build</code> (or use the official Docker image), then restart.</p>
<p>The backend still answers <code>/health</code> without signing in. Everything
under <code>/api/v1</code> needs an administrator session, which is created by
signing in through the frontend.</p>`

func serveFallbackPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(fallbackPage))
}
