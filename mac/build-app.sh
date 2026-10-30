#!/usr/bin/env bash
# Builds Homeport.app (Apple Silicon and Intel in one) into dist/, with a zip of it for releases:
#   ./build-app.sh [version]
set -euo pipefail
cd "$(dirname "$0")"
version="${1:-0.0.0}"
version="${version#v}"

swift build -c release --arch arm64 --arch x86_64
bin="$(swift build -c release --arch arm64 --arch x86_64 --show-bin-path)/Homeport"

app=dist/Homeport.app
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin" "$app/Contents/MacOS/Homeport"

# The icon, drawn at every size macOS asks for.
icons="$(mktemp -d)/Homeport.iconset"
mkdir -p "$icons"
for s in 16 32 128 256 512; do
  swift icon.swift "$s" "$icons/icon_${s}x${s}.png"
  swift icon.swift "$((s * 2))" "$icons/icon_${s}x${s}@2x.png"
done
iconutil -c icns "$icons" -o "$app/Contents/Resources/Homeport.icns"

cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Homeport</string>
  <key>CFBundleDisplayName</key><string>Homeport</string>
  <key>CFBundleIdentifier</key><string>dev.homeport.mac</string>
  <key>CFBundleExecutable</key><string>Homeport</string>
  <key>CFBundleIconFile</key><string>Homeport</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$version</string>
  <key>CFBundleVersion</key><string>$version</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.utilities</string>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST

# Signed for this machine only (no Developer ID): a copy downloaded from the internet opens with right-click → Open.
codesign --force --sign - "$app"
(cd dist && rm -f Homeport-mac.zip && ditto -c -k --keepParent Homeport.app Homeport-mac.zip)
echo "Built $app ($version)"
