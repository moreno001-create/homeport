#!/usr/bin/env bash
# Signs Mac programs with this Mac's Developer ID (or HOMEPORT_SIGN_ID), so macOS keeps the permissions it gave
# (Full Disk Access, removable drives) when a new build replaces an old one: it recognizes the team and the
# identifier, not one exact build. Without a Developer ID it signs for this machine only, as before.
# Notarizes too when a notarytool profile named "homeport" exists (xcrun notarytool store-credentials homeport).
#   mac/sign.sh IDENTIFIER PATH...
set -euo pipefail
ident="$1"; shift
id="${HOMEPORT_SIGN_ID:-$(security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -1)}"
for f in "$@"; do
  if [ -n "$id" ]; then
    codesign --force --sign "$id" --identifier "$ident" --options runtime --timestamp "$f"
  else
    codesign --force --sign - --identifier "$ident" "$f"
  fi
done
if [ -n "$id" ] && xcrun notarytool history --keychain-profile homeport >/dev/null 2>&1; then
  for f in "$@"; do
    zip="$(mktemp -d)/notarize.zip"
    ditto -c -k --keepParent "$f" "$zip"
    xcrun notarytool submit "$zip" --keychain-profile homeport --wait >/dev/null
    case "$f" in *.app) xcrun stapler staple "$f" >/dev/null ;; esac
  done
fi
echo "signed: ${id:-ad hoc} ($ident)" >&2
