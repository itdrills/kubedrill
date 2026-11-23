package sim

import (
	"fmt"
	"strings"

	"github.com/itdrills/kubedrill/internal/imagecheck"
)

// ImageVerifier looks up image references in their real registry. The
// lookup must not block; it returns imagecheck.Pending while it is running.
// *imagecheck.Checker implements it.
type ImageVerifier interface {
	Lookup(registry, repository, reference string) imagecheck.Result
}

// pullOutcome is the kubelet's decision about one simulated image pull.
type pullOutcome struct {
	profile *ImageProfile
	err     error  // PullError when the pull fails
	pending bool   // registry lookup still running: keep "Pulling"
	digest  string // real manifest digest, if known
	note    string // set when the registry was unreachable (offline fallback)
}

// decidePull combines the online registry lookup (if enabled) with the
// offline catalog. The registry is authoritative when it answers; the
// catalog is used for simulator-only images and as offline fallback.
func (c *Cluster) decidePull(image string) pullOutcome {
	if strings.TrimSpace(image) == "" || c.images == nil {
		p, err := ResolveImage(image)
		return pullOutcome{profile: p, err: err}
	}
	ref := ParseImage(image)
	if cp := catalogProfile(ref); cp != nil && cp.Simulated {
		p, err := ResolveImage(image)
		return pullOutcome{profile: p, err: err}
	}
	r := c.images.Lookup(ref.Registry, ref.Path, ref.Reference())
	switch r.Status {
	case imagecheck.Pending:
		return pullOutcome{pending: true}
	case imagecheck.Exists:
		return pullOutcome{profile: profileOf(image), digest: r.Digest}
	case imagecheck.NotFound:
		return pullOutcome{err: errImageNotFound(ref)}
	case imagecheck.Denied:
		return pullOutcome{err: errImageAccessDenied(ref)}
	default: // Unreachable
		p, err := ResolveImage(image)
		return pullOutcome{profile: p, err: err,
			note: fmt.Sprintf("Registry %s is unreachable (%s); falling back to the offline image catalog", ref.Registry, r.Detail)}
	}
}

// knownDigest returns the real digest of an image if a registry lookup
// already found it (never starts a new lookup for simulator-only images).
func (c *Cluster) knownDigest(image string) string {
	if c.images == nil {
		return ""
	}
	ref := ParseImage(image)
	if cp := catalogProfile(ref); cp != nil && cp.Simulated {
		return ""
	}
	if ref.Digest != "" {
		return ref.Digest
	}
	if r := c.images.Lookup(ref.Registry, ref.Path, ref.Reference()); r.Status == imagecheck.Exists {
		return r.Digest
	}
	return ""
}
