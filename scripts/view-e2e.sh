#!/usr/bin/env bash
# view-e2e.sh: headless-Chrome check for witness view. Builds fixtures, runs Chrome on each,
# checks for DOM markers, escaped text, and no NaN/undefined rendered from a Model key the page
# misread. NOT part of `make test` (it needs Chrome); TestEveryModelKeyIsReadByThePage is the
# in-suite guard for renamed keys.

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
go test -tags e2e ./internal/view -run TestWriteE2EFixtures -count=1 -args -out "$tmpdir"

# Check one file: run Chrome headless, fail on Uncaught in stderr or missing markers.
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

  # Run Chrome headless and capture stdout (DOM) and stderr separately.
  local dom
  local stderr_file
  stderr_file=$(mktemp)
  dom=$("$chrome" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
    --user-data-dir="$profile" --force-prefers-reduced-motion --virtual-time-budget=3000 \
    --enable-logging=stderr --v=0 --dump-dom "file://$tmpdir/$file" 2>"$stderr_file") || {
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
    if ! echo "$dom" | grep -q "$marker"; then
      echo "FAIL: $file missing marker: $marker"
      exit 1
    fi
  done

  # A Model key page.js misreads renders as "NaN% sure" or "undefined" rather than failing. Neither
  # token appears in page.js, its CSS, or any fixture, so any hit came from rendering.
  if echo "$dom" | grep -qwE 'NaN|undefined'; then
    echo "FAIL: $file renders NaN or undefined:"
    echo "$dom" | grep -oE '.{0,40}(NaN|undefined).{0,40}' | head -5
    exit 1
  fi

  # For hostile.html, verify escaping using the unique <witness-probe> tag. The whole DOM must
  # NOT contain a real <witness-probe element (JSON holds escaped <, so no literal can appear),
  # but must contain at least one &lt;witness-probe&gt; (proving the text was rendered as text).
  if [ "$file" = "hostile.html" ]; then
    # Capture grep results explicitly so failures under `set -e` don't pass silently.
    local probe_element_count
    local probe_escaped_count
    probe_element_count=$(echo "$dom" | grep -c '<witness-probe' || true)
    probe_escaped_count=$(echo "$dom" | grep -c '&lt;witness-probe&gt;' || true)

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
dom_full=$("$chrome" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
  --user-data-dir="$tmpdir/profile-full-visible" --force-prefers-reduced-motion --virtual-time-budget=3000 \
  --enable-logging=stderr --v=0 --dump-dom "file://$tmpdir/full.html" 2>/dev/null) || {
  echo "FAIL: Chrome failed for full.html visibility check"
  exit 1
}
if echo "$dom_full" | grep -q '<section[^>]*data-page="overview"[^>]*hidden'; then
  echo "FAIL: full.html overview section has hidden attribute"
  exit 1
fi

check_file "full-changed.html" 'data-page="changed"'

# full-changed.html: changed section must be visible (no hidden attribute) and have a confidence point.
dom_changed=$("$chrome" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
  --user-data-dir="$tmpdir/profile-changed-visible" --force-prefers-reduced-motion --virtual-time-budget=3000 \
  --enable-logging=stderr --v=0 --dump-dom "file://$tmpdir/full-changed.html" 2>/dev/null) || {
  echo "FAIL: Chrome failed for full-changed.html visibility check"
  exit 1
}
if echo "$dom_changed" | grep -q '<section[^>]*data-page="changed"[^>]*hidden'; then
  echo "FAIL: full-changed.html changed section has hidden attribute"
  exit 1
fi
if ! echo "$dom_changed" | grep -q 'class="pt"'; then
  echo "FAIL: full-changed.html missing confidence point (class=\"pt\")"
  exit 1
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
