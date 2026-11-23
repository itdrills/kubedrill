package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// The OpenAPI v3 documents are the unmodified upstream Kubernetes specs
// (Apache-2.0, see internal/api/openapi/README.md). They enable
// `kubectl explain` and tell kubectl that the server performs field
// validation. The simulator itself implements only a subset of the
// operations described there.
//
//go:embed openapi/*.json.gz
var specFS embed.FS

type spec struct {
	data []byte
	hash string
}

var (
	specOnce sync.Once
	specs    map[string]spec // key: "api/v1", "apis/apps/v1", ...
)

func loadSpecs() {
	specs = map[string]spec{}
	entries, _ := specFS.ReadDir("openapi")
	for _, e := range entries {
		raw, err := specFS.ReadFile("openapi/" + e.Name())
		if err != nil {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			continue
		}
		data, err := io.ReadAll(zr)
		if err != nil {
			continue
		}
		// api__v1.json.gz -> api/v1 ; apis__apps__v1.json.gz -> apis/apps/v1
		name := strings.TrimSuffix(e.Name(), ".json.gz")
		name = strings.ReplaceAll(name, "__", "/")
		sum := sha512.Sum512(data)
		specs[name] = spec{data: data, hash: strings.ToUpper(fmt.Sprintf("%x", sum[:]))}
	}
}

func serveOpenAPIV3(w http.ResponseWriter, r *http.Request) {
	specOnce.Do(loadSpecs)
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/openapi/v3"), "/")
	if p == "" {
		paths := map[string]any{}
		for name, s := range specs {
			paths[name] = map[string]string{"serverRelativeURL": "/openapi/v3/" + name + "?hash=" + s.hash}
		}
		writeJSON(w, http.StatusOK, map[string]any{"paths": paths})
		return
	}
	s, ok := specs[p]
	if !ok {
		writeStatus(w, notFoundPath())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Etag", `"`+s.hash+`"`)
	if r.URL.Query().Get("hash") != "" {
		w.Header().Set("Cache-Control", "public, immutable")
		w.Header().Set("Expires", "Thu, 01 Jan 2099 00:00:00 GMT")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.data)
}
