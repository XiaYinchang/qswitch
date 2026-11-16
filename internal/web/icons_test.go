package web

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestPublicIconsServeActualArtworkWithExplicitTypes(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	for path, contentType := range iconTypes {
		t.Run(path, func(t *testing.T) {
			w := do(h, proxied(http.MethodGet, path))
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != contentType || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("asset response: %d %v", w.Code, w.Header())
			}
			body := w.Body.Bytes()
			switch path {
			case "/favicon.svg":
				if !bytes.Contains(body, []byte("<svg")) || !bytes.Contains(body, []byte("</svg>")) {
					t.Fatal("SVG route did not serve an SVG image")
				}
			case "/favicon.ico":
				if len(body) < 22 || !bytes.Equal(body[:4], []byte{0, 0, 1, 0}) {
					t.Fatal("ICO route did not serve an icon container")
				}
				count := int(binary.LittleEndian.Uint16(body[4:6]))
				if count == 0 || len(body) < 6+16*count {
					t.Fatal("ICO image directory is missing")
				}
				for i := 0; i < count; i++ {
					entry := body[6+16*i : 22+16*i]
					size := uint64(binary.LittleEndian.Uint32(entry[8:12]))
					offset := uint64(binary.LittleEndian.Uint32(entry[12:16]))
					if size == 0 || offset < uint64(6+16*count) || offset+size > uint64(len(body)) {
						t.Fatal("ICO directory points outside its image data")
					}
				}
			case "/apple-touch-icon.png", "/icon-192.png", "/icon-512.png":
				config, err := png.DecodeConfig(bytes.NewReader(body))
				want := map[string]int{"/apple-touch-icon.png": 180, "/icon-192.png": 192, "/icon-512.png": 512}[path]
				if err != nil || config.Width != want || config.Height != want {
					t.Fatalf("PNG dimensions: %+v, error=%v", config, err)
				}
			case "/site.webmanifest":
				var manifest struct {
					Name     string `json:"name"`
					StartURL string `json:"start_url"`
					Scope    string `json:"scope"`
					Icons    []struct {
						Src   string `json:"src"`
						Sizes string `json:"sizes"`
						Type  string `json:"type"`
					} `json:"icons"`
				}
				if err := json.Unmarshal(body, &manifest); err != nil || manifest.Name != "qswitch" || manifest.StartURL != "./" || manifest.Scope != "./" || len(manifest.Icons) != 2 {
					t.Fatalf("manifest does not support prefixed installations: %s, error=%v", body, err)
				}
				for _, icon := range manifest.Icons {
					u, err := url.Parse(icon.Src)
					if err != nil || u.IsAbs() || strings.HasPrefix(u.Path, "/") || u.Query().Get("v") == "" || icon.Type != "image/png" {
						t.Fatal("manifest icon is not a versioned relative PNG")
					}
					image := do(h, proxied(http.MethodGet, "/"+icon.Src))
					if image.Code != http.StatusOK || !strings.Contains(image.Header().Get("Cache-Control"), "immutable") {
						t.Fatal("manifest icon URL does not resolve to its versioned artwork")
					}
				}
			}
		})
	}
}

func TestIconCachingRevalidatesStableURLsAndVersionsCurrentContent(t *testing.T) {
	h := tokenServer("synthetic-token").Handler()
	for path := range iconTypes {
		w := do(h, proxied(http.MethodGet, path))
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=0, must-revalidate" || w.Header().Get("ETag") == "" {
			t.Fatalf("unversioned asset cannot refresh: %s %v", path, w.Header())
		}
		version := strings.Trim(w.Header().Get("ETag"), `"`)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			versioned := do(h, proxied(method, path+"?v="+version))
			if versioned.Code != http.StatusOK || !strings.Contains(versioned.Header().Get("Cache-Control"), "immutable") {
				t.Fatalf("versioned asset was not cached: %s %v", path, versioned.Header())
			}
			if method == http.MethodHead && versioned.Body.Len() != 0 {
				t.Fatal("HEAD returned asset bytes")
			}
		}
		r := proxied(http.MethodGet, path)
		r.Header.Set("If-None-Match", w.Header().Get("ETag"))
		if response := do(h, r); response.Code != http.StatusNotModified || response.Body.Len() != 0 {
			t.Fatal("asset revalidation did not return 304")
		}
		if stale := do(h, proxied(http.MethodGet, path+"?v=old-content")); strings.Contains(stale.Header().Get("Cache-Control"), "immutable") {
			t.Fatal("old asset version was incorrectly made immutable")
		}
	}
}

func TestHomeAndPasswordPageLinkPublicVersionedIconsAtTheirMountPoint(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	for _, test := range []struct {
		name   string
		path   string
		prefix string
	}{
		{"local homepage", "/", ""},
		{"proxied password page", "/login", "/qswitch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := local(http.MethodGet, test.path)
			if test.prefix != "" {
				r = proxied(http.MethodGet, test.path)
			}
			page := do(h, r)
			if page.Code != http.StatusOK || strings.Contains(page.Body.String(), iconPlaceholder) {
				t.Fatal("page icon links were not rendered")
			}
			csp := page.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "manifest-src 'self'") {
				t.Fatal("page CSP blocks its own branding")
			}
			links := regexp.MustCompile(`<link[^>]+href="([^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
			if len(links) != 4 {
				t.Fatalf("expected favicon, SVG, touch icon and manifest: %d", len(links))
			}
			for _, link := range links {
				u, err := url.Parse(link[1])
				if err != nil || !strings.HasPrefix(u.Path, test.prefix+"/") || u.Query().Get("v") == "" {
					t.Fatalf("incorrect mounted icon URL: %s", link[1])
				}
				asset := do(h, proxied(http.MethodGet, strings.TrimPrefix(u.RequestURI(), test.prefix)))
				if asset.Code != http.StatusOK {
					t.Fatalf("password-page icon requires login: %s", link[1])
				}
			}
		})
	}
}

func TestPublicIconsDoNotExpandAPIAccessOrBypassOriginChecks(t *testing.T) {
	s, _, _ := passwordServer(t, "127.0.0.1:7432", "")
	h := s.Handler()
	for _, path := range []string{"/api/overview", "/api/session", "/favicon.svg/extra", "/static/index.html"} {
		if w := do(h, proxied(http.MethodGet, path)); w.Code != http.StatusUnauthorized {
			t.Fatalf("branding exemption exposed %s: %d", path, w.Code)
		}
	}
	for path := range iconTypes {
		if w := do(h, proxied(http.MethodPost, path)); w.Code != http.StatusUnauthorized {
			t.Fatalf("branding exemption accepted a write: %s %d", path, w.Code)
		}
		if w := do(h, local(http.MethodPost, path)); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("icon route was not read-only: %s %d", path, w.Code)
		}
	}
	r := proxied(http.MethodGet, "/favicon.svg")
	r.Header.Set("Origin", "https://other.example.invalid")
	if w := do(h, r); w.Code != http.StatusForbidden {
		t.Fatal("public icon bypassed same-origin validation")
	}
	r = local(http.MethodGet, "/favicon.ico")
	r.Host = "other.example.invalid"
	if w := do(h, r); w.Code != http.StatusForbidden {
		t.Fatal("public icon bypassed loopback Host validation")
	}
}
