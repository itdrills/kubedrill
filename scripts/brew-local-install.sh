#!/usr/bin/env bash
# Installs kubedrill through Homebrew from a local tarball, using a local tap.
# Homebrew only installs formulae from taps, so we create one that exists only
# on this machine (no GitHub repository is needed or contacted).
set -euo pipefail
cd "$(dirname "$0")/.."
TAP="kubedrill-local/tap"
command -v brew >/dev/null || { echo "Homebrew not found: https://brew.sh" >&2; exit 1; }
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 HOMEBREW_NO_ENV_HINTS=1

if [ "${1:-}" = "--uninstall" ]; then
  brew uninstall --formula "$TAP/kubedrill" 2>/dev/null || true
  brew untap "$TAP" 2>/dev/null || true
  echo "uninstalled kubedrill and removed tap $TAP"
  exit 0
fi

./scripts/brew-formula.sh
tapdir="$(brew --repository)/Library/Taps/${TAP%/*}/homebrew-${TAP#*/}"
if [ ! -d "$tapdir" ]; then
  brew tap-new --no-git "$TAP" >/dev/null
fi
mkdir -p "$tapdir/Formula"
cp dist/kubedrill.rb "$tapdir/Formula/kubedrill.rb"
if brew list --formula "$TAP/kubedrill" >/dev/null 2>&1; then
  brew reinstall --formula "$TAP/kubedrill"
else
  brew install --formula "$TAP/kubedrill"
fi
brew test "$TAP/kubedrill"
echo
echo "installed: $(command -v kubedrill) -> $(kubedrill version)"
