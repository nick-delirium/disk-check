#!/bin/sh
# Regenerates packaging/AppIcon.icns from packaging/icon.svg (needs Google Chrome for rendering).
set -eu
cd "$(dirname "$0")"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
printf '<html><body style="margin:0;background:transparent">%s</body></html>' "$(cat icon.svg)" > "$TMP/icon.html"
"$CHROME" --headless=new --disable-gpu --user-data-dir="$TMP/profile" --hide-scrollbars \
  --default-background-color=00000000 --window-size=1024,1024 \
  --screenshot="$TMP/icon-1024.png" "file://$TMP/icon.html" 2>/dev/null
SET="$TMP/AppIcon.iconset"
mkdir -p "$SET"
for s in 16 32 128 256 512; do
  sips -z $s $s "$TMP/icon-1024.png" --out "$SET/icon_${s}x${s}.png" >/dev/null
  d=$((s * 2))
  sips -z $d $d "$TMP/icon-1024.png" --out "$SET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$SET" -o AppIcon.icns
cp "$TMP/icon-1024.png" icon-1024.png
echo "wrote packaging/AppIcon.icns"
