package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// asset is a static file as served: its content and a short hash of it.
type asset struct {
	body    []byte
	version string
}

// assets are the static files by name. Stylesheets are served with the
// files they name in url(...) linked by version, like pages link the
// stylesheet, so a font is fetched once and then cached for good.
var assets = loadAssets()

// assetVersions maps each static file's name to a short hash of its
// content as served. Pages link assets with it (assetURL), so a new
// release's files are fetched at once while unchanged ones stay cached.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	for name, a := range assets {
		out[name] = a.version
	}
	return out
}()

// cssURL matches a url(...) of a stylesheet that names a file beside it.
var cssURL = regexp.MustCompile(`url\(([a-z0-9-]+\.[a-z0-9]+)\)`)

func loadAssets() map[string]asset {
	out := map[string]asset{}
	var css []string
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		if path.Ext(name) == ".css" {
			css = append(css, name)
		}
		out[name] = asset{body: b, version: hashOf(b)}
		return nil
	})
	if err != nil {
		panic(err)
	}
	for _, name := range css {
		body := cssURL.ReplaceAllFunc(out[name].body, func(m []byte) []byte {
			file := string(cssURL.FindSubmatch(m)[1])
			a, ok := out[file]
			if !ok || path.Ext(file) == ".css" {
				panic("web: " + name + " names " + file + ", which isn't a static file")
			}
			return []byte("url(/static/" + file + "?v=" + a.version + ")")
		})
		out[name] = asset{body: body, version: hashOf(body)}
	}
	return out
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// assetTypes fixes the content types of the assets, rather than trusting
// the host's MIME tables.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
}

// assetURL is the versioned URL of a static file, e.g.
// "/static/app.js?v=3f2a…".
func assetURL(name string) string {
	return "/static/" + name + "?v=" + assetVersions[name]
}

// assetRoutes serve the embedded stylesheet, script, fonts and icon. A
// request with the current version may be cached for good; any other must
// be revalidated. /favicon.ico, which browsers ask for on their own, sends
// them to the icon.
func (s *Server) assetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, assetURL("favicon.svg"), http.StatusFound)
	})
	mux.HandleFunc("GET /static/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		a, ok := assets[name]
		if !ok {
			s.notFound(w, r)
			return
		}
		if r.URL.Query().Get("v") == a.version {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Header().Set("ETag", `"`+a.version+`"`)
		if ct, ok := assetTypes[path.Ext(name)]; ok {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
	})
}
