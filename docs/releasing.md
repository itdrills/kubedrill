# Releasing KubeDrill

A release is triggered by pushing a tag `vX.Y.Z`. The workflow
[`.github/workflows/release.yml`](../.github/workflows/release.yml) then

1. runs GoReleaser: tests, binaries for macOS/Linux (amd64, arm64), tarballs,
   `checksums.txt`, `.deb` and signed `.rpm` packages, the GitHub release and the
   Homebrew cask in [`itskills/homebrew-tap`](https://github.com/itskills/homebrew-tap),
2. builds signed apt and dnf repositories from the packages
   ([`scripts/publish-repos.sh`](../scripts/publish-repos.sh)) and pushes them to
   the `gh-pages` branch, served at <https://itskills.github.io/kubedrill/>.

## One-time setup

### 1. Repositories

- `itskills/kubedrill` – public, this code.
- `itskills/homebrew-tap` – public, may be empty (a README is enough). GoReleaser
  writes `Casks/kubedrill.rb` into it. Homebrew finds it as `itskills/tap`.

### 2. Signing key for apt/dnf (GPG)

Create a dedicated key on your machine (not your personal key). Use an RSA key –
older `rpm` versions (RHEL 8) cannot verify EdDSA signatures:

```sh
gpg --quick-gen-key "KubeDrill Release Signing <ADDRESS>" rsa4096 sign 3y
gpg --list-secret-keys --keyid-format long          # note the fingerprint
```

Use a role address you are happy to publish (it is visible in `gpg.key`).
Then store the key as repository secrets (the private key never touches disk
in CI except a temp file that is deleted after the build):

```sh
gpg --armor --export-secret-keys <FINGERPRINT> | gh secret set GPG_PRIVATE_KEY --repo itskills/kubedrill
gh secret set GPG_PASSPHRASE --repo itskills/kubedrill        # prompts for the passphrase
```

Keep an offline backup of the key and its revocation certificate
(`~/.gnupg/openpgp-revocs.d/`). Losing it means every user has to import a new
key.

### 3. Token for the Homebrew tap

The default `GITHUB_TOKEN` can only write to `itskills/kubedrill`. Create a
**fine-grained personal access token**:

- Resource owner: `itskills` (if the organization blocks fine-grained tokens,
  allow them under *Organization settings → Personal access tokens*),
- Repository access: only `itskills/homebrew-tap`,
- Permissions: *Contents: Read and write*,
- an expiry date you put in your calendar.

```sh
gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo itskills/kubedrill
```

### 4. Actions permissions

*Settings → Actions → General*: allow GitHub Actions. The workflows request the
permissions they need (`contents: write` for releases and `gh-pages`).

### 5. Private vulnerability reporting

*Settings → Security → Private vulnerability reporting → Enable* (referenced by
[SECURITY.md](../SECURITY.md)).

## Cutting a release

```sh
make test e2e lint snapshot          # local dry run, nothing is published
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

After the **first** release the `gh-pages` branch exists. Enable GitHub Pages
once: *Settings → Pages → Build and deployment → Deploy from a branch →
`gh-pages` / `(root)`*. Later releases update it automatically.

## Verifying a release

```sh
# macOS
brew install itskills/tap/kubedrill && kubedrill version

# Ubuntu/Debian and Fedora: see README "Installation"; then
kubedrill version && kubedrill doctor
```

The packages contain only `/usr/bin/kubedrill` and documentation; they do not
start services or modify any configuration.

## Rotating the signing key

Generate a new key, update both secrets, release. Users must import the new
`gpg.key` (apt: re-run the `curl … | gpg --dearmor` line; dnf asks on the next
upgrade). Announce the new fingerprint in the release notes.
