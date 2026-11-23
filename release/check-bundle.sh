#!/bin/sh
# Checks the bundle ggui build made against ggui.toml: that it is there
# under the app's name, carries the identifier and the version the file
# gives it, and has its icon. A ggui.toml written over by the template
# would otherwise ship an app with a placeholder for a name.
set -eu

out="${1:-build}"
field() { sed -n "s/^$1 = \"\\(.*\\)\"\$/\\1/p" ggui.toml; }
name=$(field name)
id=$(field id)
version=$(field version)
app="$out/$name.app"
plist="$app/Contents/Info.plist"

fail() { echo "check-bundle: $1" >&2; exit 1; }
[ -f "$plist" ] || fail "no bundle at $app"
case "$id" in
com.example.*|"") fail "ggui.toml's id is a placeholder: '$id'" ;;
esac
# The value of a string key of Info.plist, read as text: the bundle is made
# on Linux too, which has no PlistBuddy.
got() {
	awk -v key="<key>$1</key>" '
		found { gsub(/^[ \t]*<string>|<\/string>[ \t]*$/, ""); print; exit }
		index($0, key) { found = 1 }
	' "$plist"
}
[ "$(got CFBundleIdentifier)" = "$id" ] || fail "the bundle's identifier is $(got CFBundleIdentifier), not $id"
[ "$(got CFBundleShortVersionString)" = "$version" ] || fail "the bundle's version is $(got CFBundleShortVersionString), not $version"
[ -f "$app/Contents/Resources/AppIcon.icns" ] || fail "the bundle has no icon"
[ -f "$app/Contents/_CodeSignature/CodeResources" ] || fail "the bundle is not signed, so macOS would call it damaged once downloaded"
exe="$app/Contents/MacOS/$(got CFBundleExecutable)"
[ -x "$exe" ] || fail "no executable at $exe"
if [ "$(field renderer)" = "zig" ]; then
	[ -f "$app/Contents/Frameworks/libggfx_driver.dylib" ] || fail "the bundle has no Contents/Frameworks/libggfx_driver.dylib"
fi
for helper in \
	"$app/Contents/Helpers/docker" \
	"$app/Contents/Helpers/cli-plugins/docker-buildx" \
	"$app/Contents/Helpers/cli-plugins/docker-compose"
do
	[ -x "$helper" ] || fail "the bundle is missing bundled Docker CLI binary $helper"
done
ent="com.apple.security.virtualization"
if command -v codesign >/dev/null 2>&1; then
	codesign --verify --deep --strict "$app" ||
		fail "codesign --verify --deep --strict failed for $app"
	codesign -d --entitlements - --xml "$app" 2>/dev/null | grep -Fq "<key>$ent</key>" ||
		fail "the bundle's signature is missing the $ent entitlement"
else
	# On Linux (where codesign is absent), machosign embeds the XML
	# entitlements plist verbatim in the main executable's signature blob.
	LC_ALL=C grep -Fqa "<key>$ent</key>" "$exe" ||
		fail "the bundle's executable is missing the $ent entitlement"
fi
echo "check-bundle: $name $version ($id)"
