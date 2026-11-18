#!/bin/sh
# Checks the bundle ggui build made against ggui.toml: that it is there
# under the app's name, carries the identifier and the version the file
# gives it, and has its icon. A ggui.toml written over by the template
# would otherwise ship an app with a placeholder for a name.
set -eu

field() { sed -n "s/^$1 = \"\\(.*\\)\"\$/\\1/p" ggui.toml; }
name=$(field name)
id=$(field id)
version=$(field version)
plist="build/$name.app/Contents/Info.plist"

fail() { echo "check-bundle: $1" >&2; exit 1; }
[ -f "$plist" ] || fail "no bundle at build/$name.app"
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
[ -f "build/$name.app/Contents/Resources/AppIcon.icns" ] || fail "the bundle has no icon"
[ -f "build/$name.app/Contents/_CodeSignature/CodeResources" ] || fail "the bundle is not signed, so macOS would call it damaged once downloaded"
echo "check-bundle: $name $version ($id)"
