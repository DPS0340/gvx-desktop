# Installs GVX Desktop on Windows for the current user, without asking for
# an administrator:
#
#   irm https://raw.githubusercontent.com/ironpark/gvx-desktop/main/install.ps1 | iex
#
# It takes the newest release's zip for this PC (amd64 or arm64) from
# GitHub, checks it against the SHA-256 published beside it, unpacks it
# into %LOCALAPPDATA%\Programs\GVX Desktop, and puts the program in the
# Start menu. Run again, it updates what is there. To install a version
# other than the newest, set $env:GVX_DESKTOP_VERSION, to 0.1.0 say, first.
#
# Everything is inside a script block, so that piped into iex it leaves no
# variable behind and a failure ends it without ending the session.
& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue' # the progress bar slows a download many times over
  # Windows PowerShell 5.1 does not offer GitHub TLS 1.2 by itself.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

  $repo = 'ironpark/gvx-desktop'
  $app = 'GVX Desktop'
  $headers = @{ 'User-Agent' = 'gvx-desktop-install' }

  if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') {
    $arch = 'arm64'
  } elseif ([Environment]::Is64BitOperatingSystem) {
    $arch = 'amd64'
  } else {
    throw "$app is built for 64-bit Windows, on amd64 or arm64; this PC is neither."
  }

  $api = "https://api.github.com/repos/$repo/releases/latest"
  if ($env:GVX_DESKTOP_VERSION) {
    $api = "https://api.github.com/repos/$repo/releases/tags/v$($env:GVX_DESKTOP_VERSION.TrimStart('v'))"
  }
  $release = Invoke-RestMethod -Uri $api -Headers $headers
  $zip = $release.assets | Where-Object { $_.name -like "*-windows-$arch.zip" } | Select-Object -First 1
  if (-not $zip) {
    throw "Release $($release.tag_name) has no program for Windows on $arch."
  }
  $sum = $release.assets | Where-Object { $_.name -eq "$($zip.name).sha256" } | Select-Object -First 1
  if (-not $sum) {
    throw "Release $($release.tag_name) publishes no checksum for $($zip.name), so the download cannot be checked."
  }

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("gvx-desktop-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Write-Host "Downloading $app $($release.tag_name) for $arch..."
    $zipFile = Join-Path $tmp $zip.name
    $sumFile = Join-Path $tmp $sum.name
    Invoke-WebRequest -Uri $zip.browser_download_url -OutFile $zipFile -Headers $headers -UseBasicParsing
    Invoke-WebRequest -Uri $sum.browser_download_url -OutFile $sumFile -Headers $headers -UseBasicParsing

    $want = ((Get-Content -Path $sumFile -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
    $got = (Get-FileHash -Path $zipFile -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($got -ne $want) {
      throw "$($zip.name) does not match its checksum: got $got, the release says $want."
    }

    # A program that is running cannot be written over; it may be in the
    # tray with no window to close.
    Get-Process -Name $app -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

    $dir = Join-Path $env:LOCALAPPDATA "Programs\$app"
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Expand-Archive -Path $zipFile -DestinationPath $dir -Force
    $exe = Join-Path $dir "$app.exe"
    if (-not (Test-Path $exe)) {
      throw "The archive held no $app.exe."
    }

    $menu = [Environment]::GetFolderPath('Programs')
    New-Item -ItemType Directory -Path $menu -Force | Out-Null
    $shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut((Join-Path $menu "$app.lnk"))
    $shortcut.TargetPath = $exe
    $shortcut.WorkingDirectory = $dir
    $shortcut.Description = 'Run and manage gvx sandboxes'
    $shortcut.Save()

    Write-Host "$app $($release.tag_name) is installed in $dir"
    Write-Host "Open it from the Start menu, or run: & '$exe'"
  } finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}
