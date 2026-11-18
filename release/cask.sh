#!/bin/sh
# Writes the Homebrew cask of a release to standard output:
#
#	release/cask.sh VERSION SHA256 [adhoc]
#
# VERSION is the release without its v, SHA256 that of its disk image.
# adhoc says the app is signed ad hoc rather than with a Developer ID, which
# Gatekeeper does not open as downloaded: the cask then takes the quarantine
# off the app once it is installed. The release workflow puts the cask in
# ironpark/homebrew-tap.
set -eu

version=${1:?usage: cask.sh VERSION SHA256 [adhoc]}
sha256=${2:?usage: cask.sh VERSION SHA256 [adhoc]}
signing=${3:-}

cat <<RUBY
# Written by the release workflow of github.com/ironpark/gvx-desktop; edits
# made here are overwritten by the next release.
cask "gvx-desktop" do
  version "$version"
  sha256 "$sha256"

  url "https://github.com/ironpark/gvx-desktop/releases/download/v#{version}/gvx-desktop-#{version}-macos-arm64.dmg"
  name "gvx Desktop"
  desc "Run and manage gvx sandboxes: Linux containers with no VM or daemon"
  homepage "https://github.com/ironpark/gvx-desktop"

  depends_on arch: :arm64
  depends_on macos: ">= :ventura"

  app "gvx Desktop.app"

  zap trash: "~/Library/Application Support/gvx-desktop"
RUBY

if [ "$signing" = adhoc ]; then
	cat <<'RUBY'

  # The app is signed ad hoc, not with an Apple Developer ID: macOS would
  # refuse to open it while it carries the quarantine of a download.
  postflight do
    system_command "/usr/bin/xattr",
                   args: ["-dr", "com.apple.quarantine", "#{appdir}/gvx Desktop.app"]
  end
RUBY
fi

echo end
