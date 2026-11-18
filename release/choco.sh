#!/bin/sh
# Writes the Chocolatey package of a release into a directory, for choco
# pack to make a .nupkg of:
#
#	release/choco.sh VERSION SHA256_AMD64 SHA256_ARM64 DIR
#
# VERSION is the release without its v, and the checksums are those of its
# two Windows zips. The package holds no program: installing it downloads
# the zip for the PC from the GitHub release, checks it, and puts the
# program in the Start menu. The app's name is read from ggui.toml, so run
# it from the repository's root.
set -eu

usage='usage: choco.sh VERSION SHA256_AMD64 SHA256_ARM64 DIR'
version=${1:?$usage}
amd64=${2:?$usage}
arm64=${3:?$usage}
dir=${4:?$usage}
app=$(sed -n 's/^name = "\(.*\)"$/\1/p' ggui.toml)
[ -n "$app" ] || { echo "choco.sh: ggui.toml names no app" >&2; exit 1; }

mkdir -p "$dir/tools"

cat > "$dir/gvx-desktop.nuspec" <<XML
<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2015/06/nuspec.xsd">
  <metadata>
    <id>gvx-desktop</id>
    <version>$version</version>
    <title>$app</title>
    <authors>ironpark</authors>
    <owners>ironpark</owners>
    <projectUrl>https://github.com/ironpark/gvx-desktop</projectUrl>
    <packageSourceUrl>https://github.com/ironpark/gvx-desktop/tree/main/release</packageSourceUrl>
    <projectSourceUrl>https://github.com/ironpark/gvx-desktop</projectSourceUrl>
    <docsUrl>https://github.com/ironpark/gvx-desktop#readme</docsUrl>
    <bugTrackerUrl>https://github.com/ironpark/gvx-desktop/issues</bugTrackerUrl>
    <releaseNotes>https://github.com/ironpark/gvx-desktop/releases/tag/v$version</releaseNotes>
    <iconUrl>https://raw.githubusercontent.com/ironpark/gvx-desktop/v$version/assets/icon.png</iconUrl>
    <requireLicenseAcceptance>false</requireLicenseAcceptance>
    <tags>gvx containers sandbox gvisor docker kubernetes desktop</tags>
    <summary>Run and manage gvx sandboxes: Linux containers with no VM or daemon</summary>
    <description><![CDATA[A desktop app for [gvx](https://github.com/gfire-sigs/gvx): it runs Linux containers as unprivileged gvx sandboxes, with no VM, no daemon and no root, and manages their images, volumes and snapshots.

The app installs gvx's \`rungv\` itself and keeps it up to date. gvx runs guests through the **Windows Hypervisor Platform** feature, which has to be turned on.

The package downloads the program for this PC, amd64 or arm64, from the project's GitHub release and checks it against the release's SHA-256.]]></description>
  </metadata>
  <files>
    <file src="tools\\**" target="tools" />
  </files>
</package>
XML

cat > "$dir/tools/chocolateyinstall.ps1" <<PS1
\$ErrorActionPreference = 'Stop'

\$version  = '$version'
\$app      = '$app'
\$toolsDir = Split-Path -Parent \$MyInvocation.MyCommand.Definition

# Chocolatey knows of 32 and 64 bits only: a PC on Arm is told apart here,
# under the emulation Chocolatey may itself be running in too.
\$arm = (\$env:PROCESSOR_ARCHITECTURE -eq 'ARM64') -or (\$env:PROCESSOR_ARCHITEW6432 -eq 'ARM64')
if (\$arm) {
  \$arch = 'arm64'
  \$sum  = '$arm64'
} else {
  \$arch = 'amd64'
  \$sum  = '$amd64'
}

Install-ChocolateyZipPackage \`
  -PackageName \$env:ChocolateyPackageName \`
  -Url64bit "https://github.com/ironpark/gvx-desktop/releases/download/v\$version/gvx-desktop-\$version-windows-\$arch.zip" \`
  -Checksum64 \$sum \`
  -ChecksumType64 'sha256' \`
  -UnzipLocation \$toolsDir

\$exe = Join-Path \$toolsDir "\$app.exe"
# The program has a window: the shim Chocolatey makes for it must not wait
# for it to end.
New-Item "\$exe.gui" -ItemType File -Force | Out-Null

\$shortcut = Join-Path ([Environment]::GetFolderPath('CommonPrograms')) "\$app.lnk"
Install-ChocolateyShortcut -ShortcutFilePath \$shortcut -TargetPath \$exe -Description 'Run and manage gvx sandboxes'
PS1

cat > "$dir/tools/chocolateyuninstall.ps1" <<PS1
\$ErrorActionPreference = 'Stop'

\$app = '$app'

# The program may be running, in the tray with no window to close.
Get-Process -Name \$app -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

\$shortcut = Join-Path ([Environment]::GetFolderPath('CommonPrograms')) "\$app.lnk"
if (Test-Path \$shortcut) { Remove-Item \$shortcut -Force }

# The start at login the app may have registered for itself.
Remove-ItemProperty -Path 'HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Run' -Name 'io.github.ironpark.gvx-desktop' -ErrorAction SilentlyContinue
PS1
