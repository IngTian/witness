#!/usr/bin/env bash
# view-e2e.sh: headless-Chrome check for witness view. Builds fixtures, runs Chrome on each,
# checks for DOM markers and escaped text. NOT part of `make test` — the JS stub is expected to
# fail on missing markers until Tasks 9–10.

set -eu

# DOM markers the client must emit (Tasks 9–10 own them):
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

  echo "Checking $file..."

  # Create temp user-data-dir for this run.
  local profile
  profile=$(mktemp -d)

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
    rm -rf "$profile"
    exit 1
  }

  # Check stderr for Uncaught errors.
  if grep -q "Uncaught" "$stderr_file"; then
    echo "FAIL: $file has Uncaught error in stderr:"
    grep "Uncaught" "$stderr_file"
    rm -f "$stderr_file"
    rm -rf "$profile"
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

  # For hostile.html, verify escaping: the data must NOT contain a real <b> element from the
  # hostile text (it must be escaped as &lt;b&gt;), but the DOM SHOULD contain the escaped text.
  if [ "$file" = "hostile.html" ]; then
    # The hostile text includes <b>, which should be escaped. Check the JSON data script does NOT
    # contain a real <b> element (other than in escaped form).
    if echo "$dom" | grep -A9999 '<script type="application/json"' | grep -B9999 '</script>' | grep -q '<b>'; then
      echo "FAIL: hostile.html contains unescaped <b> in data"
      exit 1
    fi
    # The DOM should contain the escaped form.
    if ! echo "$dom" | grep -q '&lt;b&gt;'; then
      echo "FAIL: hostile.html missing escaped &lt;b&gt; in data"
      exit 1
    fi
  fi

  rm -rf "$profile"
  echo "  ✓ $file"
}

# Check each file with its expected markers.
check_file "full.html" 'data-page="overview"' 'data-calendar' 'data-day' 'data-sentence' 'data-trait' 'data-chart="confidence"' 'data-chart="weeks"'
check_file "full-changed.html" 'data-page="changed"'
check_file "empty.html" 'data-page="overview"' 'data-calendar' 'data-day'
check_file "hostile.html" 'data-page="overview"' 'data-calendar' 'data-day' 'data-sentence' 'data-trait'

# Take screenshots if SHOTS is set.
if [ -n "${SHOTS:-}" ]; then
  echo "Taking screenshots to $SHOTS..."
  mkdir -p "$SHOTS"
  for page in full full-changed; do
    profile=$(mktemp -d)
    "$chrome" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
      --user-data-dir="$profile" --force-prefers-reduced-motion --virtual-time-budget=3000 \
      --screenshot="$SHOTS/$page.png" --window-size=1440,1000 "file://$tmpdir/$page.html" >/dev/null 2>&1 || true
    rm -rf "$profile"
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
