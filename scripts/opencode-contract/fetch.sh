#!/usr/bin/env bash
# fetch.sh <version|latest> <asset> <dest>: download one OpenCode release asset from opencode.ai into
# dest, check it against pins.txt, unpack it, and print the version (only that) on stdout.
#
# A pinned version must have a pin (none → exit 1), so CI only ever runs a binary someone hashed.
# `latest` is the newest @opencode/cli on npm: there is nothing to check it against, so its sha is
# printed for the next pin instead.
#
# It never runs the binary, not even `--version`: T11 (TestContractWindowsFirstRunLatency) times the
# binary's first run, so the version printed is the argument, not the binary's answer.
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: $0 <version|latest> <asset> <dest>" >&2; exit 2; }
version=$1 asset=$2 dest=$3
want=
if [ "$version" = latest ]; then
  version=$(npm view @opencode/cli version)
else
  want=$(awk -v v="$version" -v a="$asset" '$1 == v && $2 == a { print $3 }' "$(dirname "$0")/pins.txt")
  [ -n "$want" ] || { echo "fetch.sh: no pin for $version $asset in pins.txt" >&2; exit 1; }
fi
# Work inside dest, so no tool sees a path: on Windows dest is a D:\… path that sha256sum -c and
# tar may misread (a backslash escape, a host:file remote).
mkdir -p "$dest"
cd "$dest"
curl -fsSL --retry 3 -o "$asset" "https://opencode.ai/files/bin/$version/$asset"
if [ -n "$want" ]; then
  echo "$want  $asset" | sha256sum -c - >&2
else
  echo "fetch.sh: unpinned $version $asset sha256: $(sha256sum "$asset" | cut -d' ' -f1)" >&2
fi
case $asset in
  *.tar.gz) tar -xzf "$asset" ;;
  *.zip) 7z x -y "$asset" >&2 ;;
  *.exe) ;; # an installer: the caller runs it
  *) echo "fetch.sh: unknown asset type $asset" >&2; exit 1 ;;
esac
echo "$version"
