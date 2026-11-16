package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

const iconPlaceholder = "<!-- qswitch-icons -->"

// Keep this allowlist exact: the login page needs public branding, not access
// to arbitrary embedded files or authenticated application routes.
var iconTypes = map[string]string{
	"/favicon.svg":          "image/svg+xml",
	"/favicon.ico":          "image/x-icon",
	"/apple-touch-icon.png": "image/png",
	"/icon-192.png":         "image/png",
	"/icon-512.png":         "image/png",
	"/site.webmanifest":     "application/manifest+json",
}

func publicIconRequest(r *http.Request) bool {
	_, known := iconTypes[r.URL.Path]
	return known && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

func iconHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

func iconContent(path string) ([]byte, error) {
	body, err := staticFS.ReadFile("static" + path)
	if err != nil || path != "/site.webmanifest" {
		return body, err
	}
	// Relative URLs keep installations mounted under a proxy prefix working.
	// Version manifest icons too, so an installed shortcut can update artwork.
	for _, name := range []string{"icon-192.png", "icon-512.png"} {
		icon, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			return nil, err
		}
		body = bytes.ReplaceAll(body, []byte(`"`+name+`"`), []byte(`"`+name+"?v="+iconHash(icon)+`"`))
	}
	return body, nil
}

func iconURL(x external, path string) string {
	url := x.path(path)
	if body, err := iconContent(path); err == nil {
		url += "?v=" + iconHash(body)
	}
	return html.EscapeString(url)
}

func iconLinks(x external) string {
	return fmt.Sprintf(`<link rel="icon" href="%s" sizes="any">
<link rel="icon" href="%s" type="image/svg+xml">
<link rel="apple-touch-icon" href="%s" sizes="180x180">
<link rel="manifest" href="%s">`,
		iconURL(x, "/favicon.ico"), iconURL(x, "/favicon.svg"),
		iconURL(x, "/apple-touch-icon.png"), iconURL(x, "/site.webmanifest"))
}

func (s *Server) icon(w http.ResponseWriter, r *http.Request) {
	contentType, known := iconTypes[r.URL.Path]
	if !known {
		http.NotFound(w, r)
		return
	}
	body, err := iconContent(r.URL.Path)
	if err != nil {
		http.Error(w, "icon missing", http.StatusInternalServerError)
		return
	}
	version := iconHash(body)
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", `"`+version+`"`)
	h.Set("Cache-Control", "public, max-age=0, must-revalidate")
	if r.URL.Query().Get("v") == version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, strings.TrimPrefix(r.URL.Path, "/"), time.Time{}, bytes.NewReader(body))
}
