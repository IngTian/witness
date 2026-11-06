#!/usr/bin/env bash
# view-e2e.sh: headless-Chrome check for witness view. Writes four synthetic fixture pages
# (TestWriteE2EFixtures, which `make test` compiles but skips without -out), loads each in
# headless Chrome once, and greps the dumped DOM: no Uncaught error, every expected marker, no
# NaN/undefined rendered from a Model key the page misread, the right page visible, and hostile
# text rendered escaped. Needs Chrome, so it is NOT part of `make test` (with no Chrome it prints
# "skipped" and exits 0); TestEveryModelKeyIsReadByThePage is the in-suite guard for renamed keys.
#
# Env: CHROME=<path> picks the browser; ONLY="empty.html hostile.html" checks just those files;
# SHOTS=<dir> also writes full/full-changed screenshots there.

set -eu

# DOM markers the client must emit:
#
# | Marker                    | File(s)                      |
# |---------------------------|------------------------------|
# | data-page="overview"      | full, empty, hostile         |
# | data-page="changed"       | full-changed                 |
# | data-calendar             | full, empty, hostile         |
# | data-day                  | full, empty, hostile         |
# | data-sentence             | full, hostile                |
# | data-trait                | full, hostile                |
# | data-chart="confidence"   | full                         |
# | data-chart="weeks"        | full                         |

# Find Chrome: $CHROME, else macOS standard path, else PATH.
if [ -n "${CHROME:-}" ]; then
  chrome="$CHROME"
elif [ -f "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" ]; then
  chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
elif command -v google-chrome >/dev/null 2>&1; then
  chrome=google-chrome
elif command -v chromium >/dev/null 2>&1; then
  chrome=chromium
else
  echo "skipped: no Chrome"
  exit 0
fi

echo "Using Chrome: $chrome"

# Create temp directory for fixtures.
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# Build fixtures via the e2e test.
echo "Building fixtures..."
go test ./internal/view -run TestWriteE2EFixtures -count=1 -args -out "$tmpdir"

# Check one file: run Chrome headless, fail on Uncaught in stderr or missing markers. The DOM
# dump is kept at $tmpdir/<name>.dom for the page-specific checks below.
check_file() {
  local file="$1"
  shift
  local markers=("$@")

  # ONLY="empty.html hostile.html" checks just those files (the rest are reported as skipped).
  if [ -n "${ONLY:-}" ] && [[ " $ONLY " != *" $file "* ]]; then
    echo "  - $file skipped (not in ONLY)"
    return 0
  fi

  echo "Checking $file..."

  # Create temp user-data-dir for this run under $tmpdir so EXIT trap cleans it.
  local name
  name=$(basename "$file" .html)
  local profile="$tmpdir/profile-$name"
  mkdir -p "$profile"

  # Run Chrome headless, writing stdout (DOM) and stderr to separate files.
  local dom="$tmpdir/$name.dom"
  local stderr_file
  stderr_file=$(mktemp)
  "$chrome" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
    --user-data-dir="$profile" --force-prefers-reduced-motion --virtual-time-budget=3000 \
    --enable-logging=stderr --v=0 --dump-dom "file://$tmpdir/$file" >"$dom" 2>"$stderr_file" || {
    echo "Chrome failed for $file"
    cat "$stderr_file"
    rm -f "$stderr_file"
    exit 1
  }

  # Check stderr for Uncaught errors.
  if grep -q "Uncaught" "$stderr_file"; then
    echo "FAIL: $file has Uncaught error in stderr:"
    grep "Uncaught" "$stderr_file"
    rm -f "$stderr_file"
    exit 1
  fi
  rm -f "$stderr_file"

  # Check for markers in DOM.
  for marker in "${markers[@]}"; do
    if ! grep -q -- "$marker" "$dom"; then
      echo "FAIL: $file missing marker: $marker"
      exit 1
    fi
  done

  # A Model key page.js misreads renders as "NaN% sure" or "undefined" rather than failing. Neither
  # token appears in page.js, its CSS, or any fixture, so any hit came from rendering.
  if grep -qwE 'NaN|undefined' "$dom"; then
    echo "FAIL: $file renders NaN or undefined:"
    grep -oE '.{0,40}(NaN|undefined).{0,40}' "$dom" | head -5
    exit 1
  fi

  # For hostile.html, verify escaping using the unique <witness-probe> tag. The whole DOM must
  # NOT contain a real <witness-probe element (JSON holds escaped <, so no literal can appear),
  # but must contain at least one &lt;witness-probe&gt; (proving the text was rendered as text).
  if [ "$file" = "hostile.html" ]; then
    # Capture grep results explicitly so failures under `set -e` don't pass silently.
    local probe_element_count
    local probe_escaped_count
    probe_element_count=$(grep -c '<witness-probe' "$dom" || true)
    probe_escaped_count=$(grep -c '&lt;witness-probe&gt;' "$dom" || true)

    if [ "$probe_element_count" -ne 0 ]; then
      echo "FAIL: hostile.html contains $probe_element_count real <witness-probe element(s)"
      exit 1
    fi
    if [ "$probe_escaped_count" -eq 0 ]; then
      echo "FAIL: hostile.html missing escaped &lt;witness-probe&gt;"
      exit 1
    fi
  fi

  echo "  ✓ $file"
}

# Check each file with its expected markers.
check_file "full.html" 'data-page="overview"' 'data-calendar' 'data-day' 'data-sentence' 'data-trait' 'data-chart="confidence"' 'data-chart="weeks"'

# full.html: overview section must be visible (no hidden attribute on <section data-page="overview">).
if [ -f "$tmpdir/full.dom" ] && grep -q '<section[^>]*data-page="overview"[^>]*hidden' "$tmpdir/full.dom"; then
  echo "FAIL: full.html overview section has hidden attribute"
  exit 1
fi

check_file "full-changed.html" 'data-page="changed"'

# full-changed.html: changed section must be visible (no hidden attribute) and have a confidence point.
if [ -f "$tmpdir/full-changed.dom" ]; then
  if grep -q '<section[^>]*data-page="changed"[^>]*hidden' "$tmpdir/full-changed.dom"; then
    echo "FAIL: full-changed.html changed section has hidden attribute"
    exit 1
  fi
  if ! grep -q 'class="pt"' "$tmpdir/full-changed.dom"; then
    echo "FAIL: full-changed.html missing confidence point (class=\"pt\")"
    exit 1
  fi
fi

check_file "empty.html" 'data-page="overview"' 'data-calendar' 'data-day'
check_file "hostile.html" 'data-page="overview"' 'data-calendar' 'data-day' 'data-sentence' 'data-trait'

# Take screenshots if SHOTS is set.
if [ -n "${SHOTS:-}" ]; then
  echo "Taking screenshots to $SHOTS..."
  mkdir -p "$SHOTS"
  for page in full full-changed; do
    profile="$tmpdir/profile-screenshot-$page"
    mkdir -p "$profile"
    "$chrome" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
      --user-data-dir="$profile" --force-prefers-reduced-motion --virtual-time-budget=3000 \
      --screenshot="$SHOTS/$page.png" --window-size=1440,1000 "file://$tmpdir/$page.html" >/dev/null 2>&1 || true
    echo "  wrote $SHOTS/$page.png"
  done
fi

# Verify no Chrome processes remain (pgrep -f searches command line, so tmpdir in args).
if pgrep -f "$tmpdir" >/dev/null 2>&1; then
  echo "FAIL: Chrome processes still running with tmpdir in args"
  pgrep -lf "$tmpdir"
  exit 1
fi

echo "All checks passed."
