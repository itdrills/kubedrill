#!/usr/bin/env bash
# Builds signed apt and dnf repositories (served by GitHub Pages) from the
# .deb/.rpm files of a release.
#
# Usage: scripts/publish-repos.sh <packages-dir> <site-dir>
#
# Environment:
#   GPG_KEY_ID        fingerprint of the signing key (already in the keyring)
#   GPG_PASSPHRASE    passphrase of the key (optional)
#   SITE_URL          public URL of the site (default https://itdrills.github.io/kubedrill)
#   KEEP_VERSIONS     how many versions to keep in the repositories (default 5)
#   FORMATS           "deb rpm" (default) or a subset, e.g. for local tests
#
# Layout of <site-dir> (the gh-pages branch):
#   gpg.key                      public signing key (ASCII armored)
#   deb/dists/stable/...         apt metadata (InRelease, Release, Release.gpg, Packages)
#   deb/pool/main/*.deb
#   rpm/*.rpm, rpm/repodata/     dnf metadata, repomd.xml signed (repomd.xml.asc)
#   kubedrill.repo               dnf repository definition
#   index.html                   install instructions
set -euo pipefail

PKG_DIR="${1:?packages dir required}"
SITE="${2:?site dir required}"
: "${GPG_KEY_ID:?GPG_KEY_ID required}"
SITE_URL="${SITE_URL:-https://itdrills.github.io/kubedrill}"
KEEP_VERSIONS="${KEEP_VERSIONS:-5}"
FORMATS="${FORMATS:-deb rpm}"

gpg_sign() { # gpg_sign <args...>
  local pass=()
  if [ -n "${GPG_PASSPHRASE:-}" ]; then
    pass=(--pinentry-mode loopback --passphrase "$GPG_PASSPHRASE")
  fi
  gpg --batch --yes "${pass[@]}" --local-user "$GPG_KEY_ID" "$@"
}

# prune <dir> <glob-suffix>: keep the newest $KEEP_VERSIONS versions.
prune() {
  local dir="$1" pattern="$2"
  local versions
  versions="$(find "$dir" -maxdepth 1 -name "kubedrill*$pattern" -print \
    | sed -E 's#.*/kubedrill[_-]([0-9][^_-]*)[_-].*#\1#' | sort -uV)"
  local n
  n="$(printf '%s\n' "$versions" | grep -c . || true)"
  if [ "$n" -le "$KEEP_VERSIONS" ]; then
    return
  fi
  printf '%s\n' "$versions" | head -n "$((n - KEEP_VERSIONS))" | while read -r v; do
    echo "pruning version $v from $dir"
    find "$dir" -maxdepth 1 \( -name "kubedrill_${v}_*$pattern" -o -name "kubedrill-${v}-*$pattern" \) -delete
  done
}

mkdir -p "$SITE"
gpg --batch --yes --armor --export "$GPG_KEY_ID" > "$SITE/gpg.key"

if [[ " $FORMATS " == *" deb "* ]]; then
  echo "== apt repository"
  pool="$SITE/deb/pool/main"
  mkdir -p "$pool"
  cp "$PKG_DIR"/*.deb "$pool/"
  prune "$pool" ".deb"
  (
    cd "$SITE/deb"
    rm -rf dists/stable
    for arch in amd64 arm64; do
      d="dists/stable/main/binary-$arch"
      mkdir -p "$d"
      apt-ftparchive --arch "$arch" packages pool/main > "$d/Packages"
      gzip -9nkf "$d/Packages"
    done
    apt-ftparchive \
      -o APT::FTPArchive::Release::Origin=ITDrills \
      -o APT::FTPArchive::Release::Label=KubeDrill \
      -o APT::FTPArchive::Release::Suite=stable \
      -o APT::FTPArchive::Release::Codename=stable \
      -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
      -o APT::FTPArchive::Release::Components=main \
      -o APT::FTPArchive::Release::Description="KubeDrill - local Kubernetes simulator" \
      release dists/stable > Release.tmp
    mv Release.tmp dists/stable/Release
    gpg_sign --clearsign -o dists/stable/InRelease dists/stable/Release
    gpg_sign --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release
  )
fi

if [[ " $FORMATS " == *" rpm "* ]]; then
  echo "== dnf repository"
  mkdir -p "$SITE/rpm"
  cp "$PKG_DIR"/*.rpm "$SITE/rpm/"
  prune "$SITE/rpm" ".rpm"
  createrepo_c --update "$SITE/rpm"
  gpg_sign --armor --detach-sign -o "$SITE/rpm/repodata/repomd.xml.asc" "$SITE/rpm/repodata/repomd.xml"
  cat > "$SITE/kubedrill.repo" <<EOF
[kubedrill]
name=KubeDrill
baseurl=$SITE_URL/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$SITE_URL/gpg.key
EOF
fi

cat > "$SITE/index.html" <<EOF
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>KubeDrill packages</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:760px;margin:2rem auto;padding:0 1rem;color:#1f2328}
pre{background:#f6f8fa;padding:1rem;overflow-x:auto;border-radius:6px}code{font-size:14px}</style></head>
<body>
<h1>KubeDrill package repositories</h1>
<p>Local Kubernetes simulator: practice kubectl without a cluster.
Source, documentation and issues: <a href="https://github.com/itdrills/kubedrill">github.com/itdrills/kubedrill</a>.</p>
<h2>Debian / Ubuntu (apt)</h2>
<pre><code>sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL $SITE_URL/gpg.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubedrill.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubedrill.gpg] $SITE_URL/deb stable main" \\
  | sudo tee /etc/apt/sources.list.d/kubedrill.list
sudo apt update &amp;&amp; sudo apt install kubedrill</code></pre>
<h2>Fedora / RHEL / Rocky / Alma (dnf)</h2>
<pre><code>sudo curl -fsSL -o /etc/yum.repos.d/kubedrill.repo $SITE_URL/kubedrill.repo
sudo dnf install kubedrill</code></pre>
<h2>macOS (Homebrew)</h2>
<pre><code>brew install itdrills/tap/kubedrill</code></pre>
<p>Signing key: <a href="gpg.key">gpg.key</a> (fingerprint <code>$GPG_KEY_ID</code>).</p>
</body></html>
EOF
touch "$SITE/.nojekyll"
echo "site ready in $SITE"
