#!/bin/sh
# Builds "build/Disk Check.app". Pass --install to also copy it to /Applications.
set -eu
cd "$(dirname "$0")"
APP="build/Disk Check.app"

CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o build/disk-check .
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp build/disk-check "$APP/Contents/MacOS/disk-check"
cp packaging/Info.plist "$APP/Contents/Info.plist"
cp packaging/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
codesign --force --sign - "$APP"   # ad-hoc signature, local use only
echo "built $APP"

if [ "${1:-}" = "--install" ]; then
  rm -rf "/Applications/Disk Check.app"
  cp -R "$APP" /Applications/
  echo "installed /Applications/Disk Check.app"
fi
