package portal

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/web"
)

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// Vite names every emitted chunk name-HASH.ext below assets/, so a changed
// file always has a new name and an old one can be cached indefinitely.
var hashedAsset = regexp.MustCompile(`^assets/[^/]+-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

// staticAsset is one embedded file, prepared once: the portal bundle is
// otherwise copied, and sent uncompressed, on every page load.
type staticAsset struct {
	data, gzip     []byte // gzip is nil when compression does not pay off
	etag, gzipETag string
	contentType    string
	cacheControl   string // empty keeps the portal's no-store
}

type assetServer struct {
	files map[string]*staticAsset
}

var portalAssets = sync.OnceValue(func() *assetServer { return newAssetServer(web.Assets) })

func staticPortal(w http.ResponseWriter, r *http.Request) {
	portalAssets().serve(w, r)
}

func serveAssets(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	newAssetServer(assets).serve(w, r)
}

func newAssetServer(assets fs.FS) *assetServer {
	s := &assetServer{files: map[string]*staticAsset{}}
	_ = fs.WalkDir(assets, "dist", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") && name != "dist" {
			// The build placeholder and any other dot-entry are never served.
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			return nil
		}
		s.files[strings.TrimPrefix(name, "dist/")] = prepareAsset(strings.TrimPrefix(name, "dist/"), data)
		return nil
	})
	return s
}

func prepareAsset(name string, data []byte) *staticAsset {
	sum := sha256.Sum256(data)
	tag := base64.RawURLEncoding.EncodeToString(sum[:16])
	a := &staticAsset{data: data, etag: `"` + tag + `"`, contentType: mime.TypeByExtension(path.Ext(name))}
	if a.contentType == "" {
		a.contentType = http.DetectContentType(data)
	}
	switch {
	case strings.HasPrefix(a.contentType, "text/html"):
		// The application shell names the current bundle: always refetch it.
	case hashedAsset.MatchString(name):
		a.cacheControl = "public, max-age=31536000, immutable"
	default:
		a.cacheControl = "no-cache" // revalidate with the ETag
	}
	if compressible(a.contentType) && len(data) >= 1024 {
		var buffer bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
		_, _ = writer.Write(data)
		if writer.Close() == nil && buffer.Len() < len(data)*9/10 {
			// Each representation needs its own strong validator.
			a.gzip, a.gzipETag = buffer.Bytes(), `"`+tag+`-gzip"`
		}
	}
	return a
}

func compressible(contentType string) bool {
	media, _, _ := strings.Cut(contentType, ";")
	switch media {
	case "text/html", "text/css", "text/javascript", "text/plain", "text/markdown", "application/javascript", "application/json", "image/svg+xml":
		return true
	}
	return false
}

// acceptsGzip honors an explicit q=0 refusal; any other listing accepts it.
func acceptsGzip(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept-Encoding") {
		for _, item := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(item), ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			q := strings.ReplaceAll(strings.ToLower(params), " ", "")
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}

func (s *assetServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/portal/"))
	if strings.HasPrefix(name, ".") && name != "." {
		http.NotFound(w, r)
		return
	}
	if name == "." {
		name = "index.html"
	}
	asset := s.files[name]
	if asset == nil && path.Ext(name) == "" {
		name, asset = "index.html", s.files["index.html"]
	}
	if asset == nil {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("Content-Type", asset.contentType)
	if asset.cacheControl != "" {
		header.Set("Cache-Control", asset.cacheControl)
	}
	body, etag := asset.data, asset.etag
	if asset.gzip != nil {
		header.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			header.Set("Content-Encoding", "gzip")
			body, etag = asset.gzip, asset.gzipETag
		}
	}
	header.Set("ETag", etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}
