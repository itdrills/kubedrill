# Security policy

## Reporting a vulnerability

Please **do not open a public issue**. Report vulnerabilities privately through
GitHub: <https://github.com/itskills/kubedrill/security/advisories/new>
("Report a vulnerability"). You will get an answer within a few days.

Especially relevant for KubeDrill:

- anything that makes `kubedrill` or `kubectl --context kubedrill` reach a
  server other than the local simulator (kubeconfig handling, the `kubectl`
  wrapper, challenge commands),
- the local API server being reachable from other hosts or without the token,
- tampering with the apt/dnf repositories or the Homebrew cask.

## Supported versions

Only the latest release receives fixes.

## Verifying downloads

- Release archives: compare with `checksums.txt` of the release.
- apt and dnf: the repositories at <https://itskills.github.io/kubedrill/> are
  signed; the public key is published at
  <https://itskills.github.io/kubedrill/gpg.key> and its fingerprint on that page.
