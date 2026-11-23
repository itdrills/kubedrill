// Package imagecheck verifies that container images exist in their OCI
// registry (Docker Hub, ghcr.io, quay.io, registry.k8s.io, ...) without
// downloading them.
//
// It uses the OCI distribution API exactly like a container runtime resolves
// a reference: an anonymous bearer token (if the registry asks for one) and a
// HEAD request on /v2/<repository>/manifests/<tag-or-digest>. HEAD requests do
// not count towards the Docker Hub pull rate limit
// (https://docs.docker.com/docker-hub/usage/pulls/).
//
// Lookups are asynchronous: Lookup never blocks, it returns Pending and
// resolves the reference in the background. Results are cached in memory and
// persisted to a JSON file so that the next start works without network.
package imagecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Status is the outcome of a lookup.
type Status string

const (
	// Pending means the lookup is still running.
	Pending Status = "Pending"
	// Exists means the registry knows the tag or digest.
	Exists Status = "Exists"
	// NotFound means the repository exists but the tag or digest does not.
	NotFound Status = "NotFound"
	// Denied means the repository does not exist or requires credentials.
	Denied Status = "Denied"
	// Unreachable means the registry could not be asked (offline, timeout,
	// rate limited, server error). Callers fall back to offline heuristics.
	Unreachable Status = "Unreachable"
)

// Result describes one lookup.
type Result struct {
	Status    Status    `json:"status"`
	Digest    string    `json:"digest,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// manifestTypes are the media types a runtime accepts; registries answer
// 404 for a manifest whose type the client does not accept.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Checker resolves image references against their registries.
type Checker struct {
	// Client performs the HTTP requests (timeouts are set per lookup).
	Client *http.Client
	// Endpoint maps a registry name from an image reference to its base URL.
	// The default maps docker.io to https://registry-1.docker.io.
	Endpoint func(registry string) string
	// Timeout bounds one complete lookup (token + manifest request).
	Timeout time.Duration
	// TTLs per outcome. Unreachable results are retried after ErrorTTL.
	ExistsTTL, MissingTTL, ErrorTTL time.Duration
	// Now is the wall clock (tests).
	Now func() time.Time
	// Logf receives diagnostic messages (optional).
	Logf func(format string, args ...any)

	cachePath string
	mu        sync.Mutex
	cache     map[string]Result
	inflight  map[string]bool
	sem       chan struct{}
}

// New returns a checker that persists its cache to cachePath ("" disables
// persistence). A missing or unreadable cache file is ignored.
func New(cachePath string) *Checker {
	c := &Checker{
		Client:     &http.Client{},
		Endpoint:   DefaultEndpoint,
		Timeout:    8 * time.Second,
		ExistsTTL:  24 * time.Hour,
		MissingTTL: 10 * time.Minute,
		ErrorTTL:   30 * time.Second,
		Now:        time.Now,
		cachePath:  cachePath,
		cache:      map[string]Result{},
		inflight:   map[string]bool{},
		sem:        make(chan struct{}, 4),
	}
	if cachePath != "" {
		if b, err := os.ReadFile(cachePath); err == nil {
			_ = json.Unmarshal(b, &c.cache)
		}
	}
	return c
}

// DefaultEndpoint maps registry names to HTTPS base URLs.
func DefaultEndpoint(registry string) string {
	if registry == "docker.io" || registry == "index.docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + registry
}

func key(registry, repository, reference string) string {
	sep := ":"
	if strings.Contains(reference, ":") { // digest (sha256:...)
		sep = "@"
	}
	return registry + "/" + repository + sep + reference
}

// Lookup returns the cached result for registry/repository:reference or
// Pending, in which case the lookup runs in the background. It never blocks.
func (c *Checker) Lookup(registry, repository, reference string) Result {
	k := key(registry, repository, reference)
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.cache[k]; ok && c.Now().Sub(r.CheckedAt) < c.ttl(r.Status) {
		return r
	}
	if !c.inflight[k] {
		c.inflight[k] = true
		go c.resolve(k, registry, repository, reference)
	}
	return Result{Status: Pending}
}

func (c *Checker) ttl(s Status) time.Duration {
	switch s {
	case Exists:
		return c.ExistsTTL
	case NotFound, Denied:
		return c.MissingTTL
	}
	return c.ErrorTTL
}

func (c *Checker) resolve(k, registry, repository, reference string) {
	c.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	r := c.Check(ctx, registry, repository, reference)
	cancel()
	<-c.sem
	r.CheckedAt = c.Now()
	if c.Logf != nil {
		c.Logf("image check %s: %s %s", k, r.Status, r.Detail)
	}
	c.mu.Lock()
	c.cache[k] = r
	delete(c.inflight, k)
	persist := r.Status != Unreachable && c.cachePath != ""
	var data []byte
	if persist {
		durable := map[string]Result{}
		for kk, v := range c.cache {
			if v.Status != Unreachable {
				durable[kk] = v
			}
		}
		data, _ = json.MarshalIndent(durable, "", "  ")
	}
	c.mu.Unlock()
	if persist {
		if err := writeAtomic(c.cachePath, data); err != nil && c.Logf != nil {
			c.Logf("image cache: %v", err)
		}
	}
}

var errDenied = errors.New("denied")

// Check resolves one reference synchronously.
func (c *Checker) Check(ctx context.Context, registry, repository, reference string) Result {
	u := strings.TrimRight(c.Endpoint(registry), "/") + "/v2/" + repository + "/manifests/" + reference
	resp, err := c.head(ctx, u, "")
	if err != nil {
		return Result{Status: Unreachable, Detail: err.Error()}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		tok, err := c.token(ctx, resp.Header.Get("Www-Authenticate"), repository)
		switch {
		case errors.Is(err, errDenied):
			return Result{Status: Denied, Detail: err.Error()}
		case err != nil:
			return Result{Status: Unreachable, Detail: err.Error()}
		}
		if resp, err = c.head(ctx, u, tok); err != nil {
			return Result{Status: Unreachable, Detail: err.Error()}
		}
	}
	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		return Result{Status: Exists, Digest: resp.Header.Get("Docker-Content-Digest")}
	case code == http.StatusNotFound:
		return Result{Status: NotFound, Detail: "manifest unknown"}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return Result{Status: Denied, Detail: fmt.Sprintf("registry answered %d", code)}
	default: // 429 (rate limit), 5xx, unexpected codes
		return Result{Status: Unreachable, Detail: fmt.Sprintf("registry answered %d", code)}
	}
}

func (c *Checker) head(ctx context.Context, u, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestTypes)
	req.Header.Set("User-Agent", "kubedrill-imagecheck")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// token fetches an anonymous pull token as announced by a
// `WWW-Authenticate: Bearer realm=...,service=...,scope=...` challenge.
func (c *Checker) token(ctx context.Context, challenge, repository string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		// Basic auth or no challenge: anonymous access is impossible.
		return "", fmt.Errorf("%w: registry requires credentials", errDenied)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repository + ":pull"
	}
	q.Set("scope", scope)
	realm := params["realm"]
	sep := "?"
	if strings.Contains(realm, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+sep+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kubedrill-imagecheck")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%w: token request answered %d", errDenied, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("token request answered %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

// parseChallenge parses `Bearer realm="x",service="y",scope="z"`.
func parseChallenge(h string) (string, map[string]string) {
	params := map[string]string{}
	h = strings.TrimSpace(h)
	scheme, rest, _ := strings.Cut(h, " ")
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var v string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				v, rest = after[1:], ""
			} else {
				v, rest = after[1:1+end], after[2+end:]
			}
		} else {
			v, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return scheme, params
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".imagecache-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
