# Installs GVX Desktop on Windows for the current user, without asking for
# an administrator:
#
#   irm https://raw.githubusercontent.com/ironpark/gvx-desktop/main/install.ps1 | iex
#
# It downloads the newest release zip for this PC (amd64 or arm64) from GitHub,
# verifies its SHA-256 checksum, unpacks it into %LOCALAPPDATA%\Programs\GVX Desktop,
# and creates Start Menu and optional Desktop shortcuts via an interactive TUI.
#
# Environment overrides (optional):
#   $env:GVX_DESKTOP_VERSION          - install a specific version (e.g. '0.5.0')
#   $env:GVX_DESKTOP_LANG             - installer language: 'en', 'ko', or 'ja'
#   $env:GVX_DESKTOP_DESKTOP_SHORTCUT - '1' or '0' to default/control Desktop shortcut
#   $env:GVX_DESKTOP_LAUNCH           - '1' or '0' to auto-launch or skip launch prompt
#   $env:GVX_DESKTOP_NONINTERACTIVE   - '1' to run without interactive prompts
#
# Everything is inside a script block, so that piped into iex it leaves no
# variable behind and a failure ends it without ending the session.
# Source is pure ASCII (\uXXXX escapes) so Windows PowerShell 5.1 reads it
# identically via both `irm | iex` and `Get-Content -Raw | iex`.
& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue' # the progress bar slows a download many times over
  # Windows PowerShell 5.1 does not offer GitHub TLS 1.2 by itself.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
  try { [Console]::OutputEncoding = [Text.Encoding]::UTF8 } catch {}

  $repo = 'ironpark/gvx-desktop'
  $app = 'GVX Desktop'
  $headers = @{ 'User-Agent' = 'gvx-desktop-install' }

  # ---- i18n (English default; Korean and Japanese supported) --------------

  function Decode-Escapes([string]$s) {
    if (-not $s -or $s.IndexOf('\u') -lt 0) { return $s }
    return [regex]::Replace($s, '\\u([0-9a-fA-F]{4})', {
      param($m)
      [string][char][int]('0x' + $m.Groups[1].Value)
    })
  }

  $catalog = @{
    'en' = @{
      'lang_name' = 'English'
      'title' = 'GVX Desktop Installer'
      'checking' = 'Checking release info...'
      'arch_err' = '{0} is built for 64-bit Windows (amd64 or arm64); this PC is neither.'
      'no_asset' = 'Release {0} has no package for Windows on {1}.'
      'no_sum' = 'Release {0} publishes no checksum for {1}, so the download cannot be checked.'
      'bad_sum' = '{0} does not match its checksum: got {1}, expected {2}.'
      'no_exe' = 'The archive held no {0}.exe.'
      'mode_install' = 'Install'
      'mode_update' = 'Update'
      'meta_ver' = 'Version'
      'meta_path' = 'Location'
      'running_warn' = '{0} is currently running (PID: {1}).'
      'running_desc' = 'It must be closed before installing or updating.'
      'running_close' = 'Close {0} and continue'
      'cancel_opt' = 'Cancel installation'
      'cancelled' = 'Installation cancelled.'
      'options_title' = 'Installation options'
      'opt_startmenu' = 'Create Start Menu shortcut'
      'opt_desktop' = 'Create Desktop shortcut'
      'hint_select' = '\u2191/\u2193 Move   Enter Confirm   Esc Cancel   L Language ({0})'
      'hint_checklist' = '\u2191/\u2193 Move   Space Toggle   Enter Install   Esc Cancel   L Language ({0})'
      'hint_confirm' = '\u2191/\u2193 Move   Enter Confirm   Y/N Quick select   L Language ({0})'
      'step_download' = 'Downloading {0} {1} ({2})...'
      'done_download' = 'Downloaded {0}'
      'step_verify' = 'Verifying SHA-256 checksum...'
      'done_verify' = 'SHA-256 checksum verified'
      'step_stop' = 'Closing running {0}...'
      'done_stop' = 'Closed running {0}'
      'step_extract' = 'Installing files to {0}...'
      'done_extract' = 'Installed to {0}'
      'step_shortcuts' = 'Creating shortcuts...'
      'done_startmenu' = 'Start Menu shortcut created'
      'done_desktop' = 'Desktop shortcut created'
      'done_shortcuts_none' = 'Skipped shortcut creation'
      'complete_title' = '{0} {1} is installed!'
      'launch_prompt' = 'Launch {0} now?'
      'launch_yes' = 'Yes, launch now'
      'launch_no' = 'No, exit'
      'launching' = 'Launching {0}...'
      'run_hint' = 'Open from the Start Menu or run: & ''{0}'''
      'shortcut_desc' = 'Run and manage GVX sandboxes'
    }
    'ko' = @{
      'lang_name' = '\uD55C\uAD6D\uC5B4'
      'title' = 'GVX Desktop \uC124\uCE58 \uAD00\uB9AC\uC790'
      'checking' = '\uB9B4\uB9AC\uC2A4 \uC815\uBCF4\uB97C \uD655\uC778\uD558\uB294 \uC911...'
      'arch_err' = '{0}\uC740(\uB294) 64\uBE44\uD2B8 Windows(amd64 \uB610\uB294 arm64) \uC804\uC6A9\uC785\uB2C8\uB2E4. \uC774 PC\uB294 \uC9C0\uC6D0\uB418\uC9C0 \uC54A\uC2B5\uB2C8\uB2E4.'
      'no_asset' = '\uB9B4\uB9AC\uC2A4 {0}\uC5D0 Windows({1})\uC6A9 \uD328\uD0A4\uC9C0\uAC00 \uC5C6\uC2B5\uB2C8\uB2E4.'
      'no_sum' = '\uB9B4\uB9AC\uC2A4 {0}\uC5D0 {1}\uC758 \uCCB4\uD06C\uC12C\uC774 \uC5C6\uC5B4 \uB2E4\uC6B4\uB85C\uB4DC\uB97C \uAC80\uC99D\uD560 \uC218 \uC5C6\uC2B5\uB2C8\uB2E4.'
      'bad_sum' = '{0}\uC758 \uCCB4\uD06C\uC12C\uC774 \uC77C\uCE58\uD558\uC9C0 \uC54A\uC2B5\uB2C8\uB2E4 (\uACC4\uC0B0\uAC12: {1}, \uC608\uC0C1\uAC12: {2}).'
      'no_exe' = '\uC555\uCD95 \uD30C\uC77C\uC5D0 {0}.exe\uAC00 \uB4E4\uC5B4 \uC788\uC9C0 \uC54A\uC2B5\uB2C8\uB2E4.'
      'mode_install' = '\uC0C8\uB85C \uC124\uCE58'
      'mode_update' = '\uC5C5\uB370\uC774\uD2B8'
      'meta_ver' = '\uBC84\uC804'
      'meta_path' = '\uC124\uCE58 \uACBD\uB85C'
      'running_warn' = '{0}\uC774(\uAC00) \uD604\uC7AC \uC2E4\uD589 \uC911\uC785\uB2C8\uB2E4 (PID: {1}).'
      'running_desc' = '\uC124\uCE58\uD558\uAC70\uB098 \uC5C5\uB370\uC774\uD2B8\uD558\uB824\uBA74 \uBA3C\uC800 \uC2E4\uD589 \uC911\uC778 \uC571\uC744 \uC885\uB8CC\uD574\uC57C \uD569\uB2C8\uB2E4.'
      'running_close' = '{0} \uC885\uB8CC \uD6C4 \uACC4\uC18D \uC124\uCE58'
      'cancel_opt' = '\uC124\uCE58 \uCDE8\uC18C'
      'cancelled' = '\uC124\uCE58\uB97C \uCDE8\uC18C\uD588\uC2B5\uB2C8\uB2E4.'
      'options_title' = '\uC124\uCE58 \uC635\uC158'
      'opt_startmenu' = '\uC2DC\uC791 \uBA54\uB274 \uBC14\uB85C\uAC00\uAE30 \uC0DD\uC131'
      'opt_desktop' = '\uBC14\uD0D5 \uD654\uBA74 \uBC14\uB85C\uAC00\uAE30 \uC0DD\uC131'
      'hint_select' = '\u2191/\u2193 \uC774\uB3D9   Enter \uC120\uD0DD   Esc \uCDE8\uC18C   L \uC5B8\uC5B4 ({0})'
      'hint_checklist' = '\u2191/\u2193 \uC774\uB3D9   Space \uC804\uD658   Enter \uC124\uCE58 \uC2DC\uC791   Esc \uCDE8\uC18C   L \uC5B8\uC5B4 ({0})'
      'hint_confirm' = '\u2191/\u2193 \uC774\uB3D9   Enter \uC120\uD0DD   Y/N \uBE60\uB978 \uC120\uD0DD   L \uC5B8\uC5B4 ({0})'
      'step_download' = '{0} {1} ({2}) \uB2E4\uC6B4\uB85C\uB4DC \uC911...'
      'done_download' = '\uB2E4\uC6B4\uB85C\uB4DC \uC644\uB8CC ({0})'
      'step_verify' = 'SHA-256 \uCCB4\uD06C\uC12C \uAC80\uC99D \uC911...'
      'done_verify' = 'SHA-256 \uCCB4\uD06C\uC12C \uAC80\uC99D \uC644\uB8CC'
      'step_stop' = '\uC2E4\uD589 \uC911\uC778 {0} \uC885\uB8CC \uC911...'
      'done_stop' = '\uC2E4\uD589 \uC911\uC778 {0} \uC885\uB8CC \uC644\uB8CC'
      'step_extract' = '{0}\uC5D0 \uD30C\uC77C \uC124\uCE58 \uC911...'
      'done_extract' = '{0}\uC5D0 \uC124\uCE58 \uC644\uB8CC'
      'step_shortcuts' = '\uBC14\uB85C\uAC00\uAE30 \uC0DD\uC131 \uC911...'
      'done_startmenu' = '\uC2DC\uC791 \uBA54\uB274 \uBC14\uB85C\uAC00\uAE30 \uC0DD\uC131 \uC644\uB8CC'
      'done_desktop' = '\uBC14\uD0D5 \uD654\uBA74 \uBC14\uB85C\uAC00\uAE30 \uC0DD\uC131 \uC644\uB8CC'
      'done_shortcuts_none' = '\uBC14\uB85C\uAC00\uAE30 \uC0DD\uC131 \uAC74\uB108\uB700'
      'complete_title' = '{0} {1} \uC124\uCE58\uAC00 \uC644\uB8CC\uB418\uC5C8\uC2B5\uB2C8\uB2E4!'
      'launch_prompt' = '\uC9C0\uAE08 {0}\uC744(\uB97C) \uC2E4\uD589\uD560\uAE4C\uC694?'
      'launch_yes' = '\uC608, \uC9C0\uAE08 \uC2E4\uD589'
      'launch_no' = '\uC544\uB2C8\uC624, \uC885\uB8CC'
      'launching' = '{0} \uC2E4\uD589 \uC911...'
      'run_hint' = '\uC2DC\uC791 \uBA54\uB274\uC5D0\uC11C \uC5F4\uAC70\uB098 \uB2E4\uC74C \uBA85\uB839\uC73C\uB85C \uC2E4\uD589\uD558\uC138\uC694: & ''{0}'''
      'shortcut_desc' = 'GVX \uC0CC\uB4DC\uBC15\uC2A4 \uC2E4\uD589 \uBC0F \uAD00\uB9AC'
    }
    'ja' = @{
      'lang_name' = '\u65E5\u672C\u8A9E'
      'title' = 'GVX Desktop \u30A4\u30F3\u30B9\u30C8\u30FC\u30E9\u30FC'
      'checking' = '\u30EA\u30EA\u30FC\u30B9\u60C5\u5831\u3092\u78BA\u8A8D\u3057\u3066\u3044\u307E\u3059...'
      'arch_err' = '{0} \u306F 64 \u30D3\u30C3\u30C8 Windows (amd64 / arm64) \u5C02\u7528\u3067\u3059\u3002\u3053\u306E PC \u306F\u5BFE\u5FDC\u3057\u3066\u3044\u307E\u305B\u3093\u3002'
      'no_asset' = '\u30EA\u30EA\u30FC\u30B9 {0} \u306B\u306F Windows ({1}) \u5411\u3051\u30D1\u30C3\u30B1\u30FC\u30B8\u304C\u3042\u308A\u307E\u305B\u3093\u3002'
      'no_sum' = '\u30EA\u30EA\u30FC\u30B9 {0} \u306B\u306F {1} \u306E\u30C1\u30A7\u30C3\u30AF\u30B5\u30E0\u304C\u306A\u304F\u3001\u691C\u8A3C\u3067\u304D\u307E\u305B\u3093\u3002'
      'bad_sum' = '{0} \u306E\u30C1\u30A7\u30C3\u30AF\u30B5\u30E0\u304C\u4E00\u81F4\u3057\u307E\u305B\u3093 (\u53D6\u5F97\u5024: {1}, \u671F\u5F85\u5024: {2})\u3002'
      'no_exe' = '\u30A2\u30FC\u30AB\u30A4\u30D6\u306B {0}.exe \u304C\u542B\u307E\u308C\u3066\u3044\u307E\u305B\u3093\u3002'
      'mode_install' = '\u65B0\u898F\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB'
      'mode_update' = '\u30A2\u30C3\u30D7\u30C7\u30FC\u30C8'
      'meta_ver' = '\u30D0\u30FC\u30B8\u30E7\u30F3'
      'meta_path' = '\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u5148'
      'running_warn' = '{0} \u306F\u73FE\u5728\u5B9F\u884C\u4E2D\u3067\u3059 (PID: {1})\u3002'
      'running_desc' = '\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u307E\u305F\u306F\u30A2\u30C3\u30D7\u30C7\u30FC\u30C8\u3059\u308B\u524D\u306B\u30A2\u30D7\u30EA\u3092\u7D42\u4E86\u3059\u308B\u5FC5\u8981\u304C\u3042\u308A\u307E\u3059\u3002'
      'running_close' = '{0} \u3092\u7D42\u4E86\u3057\u3066\u7D9A\u884C'
      'cancel_opt' = '\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u3092\u30AD\u30E3\u30F3\u30BB\u30EB'
      'cancelled' = '\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u3092\u30AD\u30E3\u30F3\u30BB\u30EB\u3057\u307E\u3057\u305F\u3002'
      'options_title' = '\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u30AA\u30D7\u30B7\u30E7\u30F3'
      'opt_startmenu' = '\u30B9\u30BF\u30FC\u30C8\u30E1\u30CB\u30E5\u30FC\u306B\u30B7\u30E7\u30FC\u30C8\u30AB\u30C3\u30C8\u3092\u4F5C\u6210'
      'opt_desktop' = '\u30C7\u30B9\u30AF\u30C8\u30C3\u30D7\u306B\u30B7\u30E7\u30FC\u30C8\u30AB\u30C3\u30C8\u3092\u4F5C\u6210'
      'hint_select' = '\u2191/\u2193 \u79FB\u52D5   Enter \u6C7A\u5B9A   Esc \u30AD\u30E3\u30F3\u30BB\u30EB   L \u8A00\u8A9E ({0})'
      'hint_checklist' = '\u2191/\u2193 \u79FB\u52D5   Space \u5207\u66FF   Enter \u30A4\u30F3\u30B9\u30C8\u30FC\u30EB   Esc \u30AD\u30E3\u30F3\u30BB\u30EB   L \u8A00\u8A9E ({0})'
      'hint_confirm' = '\u2191/\u2193 \u79FB\u52D5   Enter \u6C7A\u5B9A   Y/N \u30AF\u30A4\u30C3\u30AF\u9078\u629E   L \u8A00\u8A9E ({0})'
      'step_download' = '{0} {1} ({2}) \u3092\u30C0\u30A6\u30F3\u30ED\u30FC\u30C9\u4E2D...'
      'done_download' = '\u30C0\u30A6\u30F3\u30ED\u30FC\u30C9\u5B8C\u4E86 ({0})'
      'step_verify' = 'SHA-256 \u30C1\u30A7\u30C3\u30AF\u30B5\u30E0\u3092\u691C\u8A3C\u4E2D...'
      'done_verify' = 'SHA-256 \u30C1\u30A7\u30C3\u30AF\u30B5\u30E0\u3092\u691C\u8A3C\u3057\u307E\u3057\u305F'
      'step_stop' = '\u5B9F\u884C\u4E2D\u306E {0} \u3092\u7D42\u4E86\u4E2D...'
      'done_stop' = '\u5B9F\u884C\u4E2D\u306E {0} \u3092\u7D42\u4E86\u3057\u307E\u3057\u305F'
      'step_extract' = '{0} \u306B\u30D5\u30A1\u30A4\u30EB\u3092\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u4E2D...'
      'done_extract' = '{0} \u306B\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u3057\u307E\u3057\u305F'
      'step_shortcuts' = '\u30B7\u30E7\u30FC\u30C8\u30AB\u30C3\u30C8\u3092\u4F5C\u6210\u4E2D...'
      'done_startmenu' = '\u30B9\u30BF\u30FC\u30C8\u30E1\u30CB\u30E5\u30FC\u306E\u30B7\u30E7\u30FC\u30C8\u30AB\u30C3\u30C8\u3092\u4F5C\u6210\u3057\u307E\u3057\u305F'
      'done_desktop' = '\u30C7\u30B9\u30AF\u30C8\u30C3\u30D7\u306E\u30B7\u30E7\u30FC\u30C8\u30AB\u30C3\u30C8\u3092\u4F5C\u6210\u3057\u307E\u3057\u305F'
      'done_shortcuts_none' = '\u30B7\u30E7\u30FC\u30C8\u30AB\u30C3\u30C8\u306E\u4F5C\u6210\u3092\u30B9\u30AD\u30C3\u30D7\u3057\u307E\u3057\u305F'
      'complete_title' = '{0} {1} \u306E\u30A4\u30F3\u30B9\u30C8\u30FC\u30EB\u304C\u5B8C\u4E86\u3057\u307E\u3057\u305F\uFF01'
      'launch_prompt' = '\u4ECA\u3059\u3050 {0} \u3092\u8D77\u52D5\u3057\u307E\u3059\u304B\uFF1F'
      'launch_yes' = '\u306F\u3044\u3001\u4ECA\u3059\u3050\u8D77\u52D5'
      'launch_no' = '\u3044\u3044\u3048\u3001\u7D42\u4E86'
      'launching' = '{0} \u3092\u8D77\u52D5\u3057\u3066\u3044\u307E\u3059...'
      'run_hint' = '\u30B9\u30BF\u30FC\u30C8\u30E1\u30CB\u30E5\u30FC\u304B\u3089\u958B\u304F\u304B\u3001\u6B21\u3092\u5B9F\u884C\u3057\u3066\u304F\u3060\u3055\u3044: & ''{0}'''
      'shortcut_desc' = 'GVX \u30B5\u30F3\u30C9\u30DC\u30C3\u30AF\u30B9\u306E\u5B9F\u884C\u3068\u7BA1\u7406'
    }
  }

  function Resolve-LangTag([string]$raw) {
    if (-not $raw) { return '' }
    $s = $raw.Trim().ToLowerInvariant()
    if ($s -eq 'system' -or $s -eq 'auto') { return '' }
    if ($s.StartsWith('ko')) { return 'ko' }
    if ($s.StartsWith('ja')) { return 'ja' }
    if ($s.StartsWith('en')) { return 'en' }
    return 'en'
  }

  function Detect-InitialLang {
    if ($env:GVX_DESKTOP_LANG) {
      $picked = Resolve-LangTag $env:GVX_DESKTOP_LANG
      if ($picked) { return $picked }
    }
    # Check existing GVX Desktop user settings if present.
    if ($env:APPDATA) {
      $cfgPath = Join-Path (Join-Path $env:APPDATA 'gvx-desktop') 'settings.json'
      if (Test-Path $cfgPath) {
        try {
          $cfg = Get-Content -Path $cfgPath -Raw -Encoding UTF8 | ConvertFrom-Json
          if ($cfg -and $cfg.language) {
            $picked = Resolve-LangTag ([string]$cfg.language)
            if ($picked) { return $picked }
          }
        } catch {}
      }
    }
    # Fall back to Windows UI culture (ko/ja), defaulting to English.
    try {
      $ui = [Globalization.CultureInfo]::CurrentUICulture.TwoLetterISOLanguageName
      $picked = Resolve-LangTag $ui
      if ($picked) { return $picked }
    } catch {}
    return 'en'
  }

  $langOrder = @('en', 'ko', 'ja')
  $state = @{ Lang = (Detect-InitialLang) }

  function T([string]$key, [object[]]$fmtArgs) {
    $table = $catalog[$state.Lang]
    if (-not $table -or -not $table.ContainsKey($key)) {
      $table = $catalog['en']
    }
    $text = Decode-Escapes $table[$key]
    if ($fmtArgs -and $fmtArgs.Count -gt 0) {
      return ($text -f $fmtArgs)
    }
    return $text
  }

  function Cycle-Lang {
    $idx = [Array]::IndexOf($langOrder, $state.Lang)
    $state.Lang = $langOrder[($idx + 1) % $langOrder.Count]
  }

  function Parse-BoolEnv([string]$val, [bool]$defaultVal) {
    if (-not $val) { return $defaultVal }
    switch ($val.Trim().ToLowerInvariant()) {
      '1'     { return $true }
      'true'  { return $true }
      'yes'   { return $true }
      '0'     { return $false }
      'false' { return $false }
      'no'    { return $false }
      default { return $defaultVal }
    }
  }

  # ---- Console & TUI helpers ----------------------------------------------

  $interactive = $false
  if (-not $env:CI -and -not (Parse-BoolEnv $env:GVX_DESKTOP_NONINTERACTIVE $false)) {
    try {
      if (-not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected -and $Host.UI -and $Host.UI.RawUI) {
        $null = [Console]::KeyAvailable
        $interactive = $true
      }
    } catch {
      $interactive = $false
    }
  }

  $symBar   = Decode-Escapes '\u2502'
  $symRule  = Decode-Escapes '\u2500'
  $symCheck = Decode-Escapes '\u2713'
  $symPtr   = Decode-Escapes '\u203A'
  $symDot   = Decode-Escapes '\u00B7'

  function Get-RuleWidth {
    try {
      return [Math]::Min(60, [Math]::Max(24, [Console]::WindowWidth - 4))
    } catch {
      return 56
    }
  }

  function Write-Rule([ConsoleColor]$color = [ConsoleColor]::DarkGray) {
    Write-Host ('  ' + ($symRule * (Get-RuleWidth))) -ForegroundColor $color
  }

  function Write-Banner([string]$version, [string]$archName, [string]$installDir, [bool]$isUpdate) {
    $modeKey = if ($isUpdate) { 'mode_update' } else { 'mode_install' }
    Write-Host ''
    Write-Rule ([ConsoleColor]::DarkCyan)
    Write-Host "  $symBar " -NoNewline -ForegroundColor Cyan
    Write-Host (T 'title') -NoNewline -ForegroundColor White
    Write-Host "  $symDot  " -NoNewline -ForegroundColor DarkGray
    Write-Host (T $modeKey) -ForegroundColor Cyan
    Write-Host "  $symBar " -NoNewline -ForegroundColor Cyan
    Write-Host ((T 'meta_ver') + ': ') -NoNewline -ForegroundColor DarkGray
    Write-Host "$version ($archName)" -ForegroundColor Gray
    Write-Host "  $symBar " -NoNewline -ForegroundColor Cyan
    Write-Host ((T 'meta_path') + ': ') -NoNewline -ForegroundColor DarkGray
    Write-Host $installDir -ForegroundColor Gray
    Write-Rule ([ConsoleColor]::DarkCyan)
    Write-Host ''
  }

  function Write-StepPending([string]$text) {
    Write-Host "  ... $text" -ForegroundColor DarkCyan
  }

  function Write-StepDone([string]$text) {
    Write-Host "  $symCheck " -NoNewline -ForegroundColor Green
    Write-Host $text -ForegroundColor Gray
  }

  function Clear-ConsoleSpan([int]$top, [int]$count) {
    try {
      $w = [Math]::Max(1, [Console]::WindowWidth - 1)
      $blank = ' ' * $w
      $bufH = [Console]::BufferHeight
      for ($r = 0; $r -lt $count; $r++) {
        $row = $top + $r
        if ($row -ge 0 -and $row -lt $bufH) {
          [Console]::SetCursorPosition(0, $row)
          [Console]::Write($blank)
        }
      }
      if ($top -ge 0 -and $top -lt $bufH) {
        [Console]::SetCursorPosition(0, $top)
      }
    } catch {}
  }

  function Invoke-MenuLoop([int]$expectedLines, [scriptblock]$draw, [scriptblock]$onKey) {
    $savedVisible = $true
    try {
      $savedVisible = [Console]::CursorVisible
      [Console]::CursorVisible = $false
    } catch {}
    try {
      $anchorTop = 0
      $prevSpan = $expectedLines
      $first = $true
      while ($true) {
        if ($first) {
          $startTop = 0
          try { $startTop = [Console]::CursorTop } catch {}
          & $draw
          $endTop = $startTop + $expectedLines
          try { $endTop = [Console]::CursorTop } catch {}
          $bufH = 0
          try { $bufH = [Console]::BufferHeight } catch {}
          if ($bufH -gt 0 -and $endTop -ge ($bufH - 1)) {
            $anchorTop = [Math]::Max(0, $endTop - $expectedLines)
            $prevSpan = $expectedLines
          } else {
            $anchorTop = $startTop
            $prevSpan = [Math]::Max($expectedLines, $endTop - $startTop)
          }
          $first = $false
        } else {
          Clear-ConsoleSpan $anchorTop $prevSpan
          & $draw
          try {
            $span = [Console]::CursorTop - $anchorTop
            if ($span -gt $prevSpan) { $prevSpan = $span }
          } catch {}
        }
        $key = [Console]::ReadKey($true)
        $res = & $onKey $key
        if ($null -ne $res) {
          return $res
        }
      }
    } finally {
      try { [Console]::CursorVisible = $savedVisible } catch {}
    }
  }

  # Interactive single-choice TUI selector. Returns 0-based index, or -1 on Esc.
  function Invoke-Select(
    [scriptblock]$headerFn,
    [scriptblock]$itemsFn,
    [int]$defaultIndex = 0,
    [bool]$allowYN = $false,
    [string]$hintKey = 'hint_select'
  ) {
    $ctx = @{ Index = $defaultIndex }
    $draw = {
      $hdr = @(& $headerFn)
      foreach ($line in $hdr) {
        Write-Host $line.Text -ForegroundColor $line.Color
      }
      Write-Host ''
      $items = @(& $itemsFn)
      for ($i = 0; $i -lt $items.Count; $i++) {
        if ($i -eq $ctx.Index) {
          Write-Host "  $symPtr " -NoNewline -ForegroundColor Cyan
          Write-Host $items[$i] -ForegroundColor White
        } else {
          Write-Host ('    ' + $items[$i]) -ForegroundColor DarkGray
        }
      }
      Write-Host ''
      Write-Host ('  ' + (T $hintKey @((T 'lang_name')))) -ForegroundColor DarkGray
    }
    $onKey = {
      param($key)
      $items = @(& $itemsFn)
      switch ($key.Key) {
        'UpArrow'   { $ctx.Index = ($ctx.Index + $items.Count - 1) % $items.Count; return $null }
        'K'         { $ctx.Index = ($ctx.Index + $items.Count - 1) % $items.Count; return $null }
        'DownArrow' { $ctx.Index = ($ctx.Index + 1) % $items.Count; return $null }
        'J'         { $ctx.Index = ($ctx.Index + 1) % $items.Count; return $null }
        'Enter'     { return $ctx.Index }
        'Escape'    { return -1 }
        'L'         { Cycle-Lang; return $null }
        'Y'         { if ($allowYN) { return 0 }; return $null }
        'N'         { if ($allowYN) { return 1 }; return $null }
        default     { return $null }
      }
    }
    $hdrCount = (@(& $headerFn)).Count
    $itemCount = (@(& $itemsFn)).Count
    return (Invoke-MenuLoop ($hdrCount + $itemCount + 3) $draw $onKey)
  }

  # Interactive multi-option checklist TUI. Returns hashtable with Cancelled & Checked array.
  function Invoke-Checklist(
    [scriptblock]$titleFn,
    [scriptblock]$labelsFn,
    [bool[]]$defaults
  ) {
    $checked = @()
    foreach ($d in $defaults) { $checked += $d }
    $ctx = @{ Index = 0 }
    $draw = {
      Write-Host ('  ' + (& $titleFn)) -ForegroundColor White
      Write-Host ''
      $labels = @(& $labelsFn)
      for ($i = 0; $i -lt $labels.Count; $i++) {
        $box = if ($checked[$i]) { '[x]' } else { '[ ]' }
        if ($i -eq $ctx.Index) {
          $boxColor = if ($checked[$i]) { [ConsoleColor]::Green } else { [ConsoleColor]::Gray }
          Write-Host "  $symPtr " -NoNewline -ForegroundColor Cyan
          Write-Host "$box " -NoNewline -ForegroundColor $boxColor
          Write-Host $labels[$i] -ForegroundColor White
        } else {
          $boxColor = if ($checked[$i]) { [ConsoleColor]::DarkGreen } else { [ConsoleColor]::DarkGray }
          Write-Host '    ' -NoNewline
          Write-Host "$box " -NoNewline -ForegroundColor $boxColor
          Write-Host $labels[$i] -ForegroundColor Gray
        }
      }
      Write-Host ''
      Write-Host ('  ' + (T 'hint_checklist' @((T 'lang_name')))) -ForegroundColor DarkGray
    }
    $onKey = {
      param($key)
      $n = $checked.Count
      switch ($key.Key) {
        'UpArrow'   { $ctx.Index = ($ctx.Index + $n - 1) % $n; return $null }
        'K'         { $ctx.Index = ($ctx.Index + $n - 1) % $n; return $null }
        'DownArrow' { $ctx.Index = ($ctx.Index + 1) % $n; return $null }
        'J'         { $ctx.Index = ($ctx.Index + 1) % $n; return $null }
        'Spacebar'  { $checked[$ctx.Index] = -not $checked[$ctx.Index]; return $null }
        'Enter'     { return @{ Cancelled = $false; Checked = $checked } }
        'Escape'    { return @{ Cancelled = $true; Checked = $checked } }
        'L'         { Cycle-Lang; return $null }
        default     { return $null }
      }
    }
    $labelCount = (@(& $labelsFn)).Count
    return (Invoke-MenuLoop ($labelCount + 4) $draw $onKey)
  }

  # ---- Process & Shortcut helpers -----------------------------------------

  function Get-RunningApp {
    @(Get-Process -Name @($app, 'gvx-desktop') -ErrorAction SilentlyContinue)
  }

  function Stop-RunningApp {
    $procs = Get-RunningApp
    if ($procs.Count -eq 0) { return }
    foreach ($p in $procs) {
      try {
        if ($p.MainWindowHandle -ne [IntPtr]::Zero) {
          $null = $p.CloseMainWindow()
        }
      } catch {}
    }
    $deadline = (Get-Date).AddSeconds(2)
    while ((Get-RunningApp).Count -gt 0 -and (Get-Date) -lt $deadline) {
      Start-Sleep -Milliseconds 150
    }
    foreach ($p in (Get-RunningApp)) {
      Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    }
    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-RunningApp).Count -gt 0 -and (Get-Date) -lt $deadline) {
      Start-Sleep -Milliseconds 200
    }
  }

  function New-AppShortcut([string]$lnkPath, [string]$targetExe, [string]$workDir, [string]$desc) {
    $parent = Split-Path -Path $lnkPath -Parent
    if ($parent -and -not (Test-Path $parent)) {
      New-Item -ItemType Directory -Path $parent -Force | Out-Null
    }
    $shell = New-Object -ComObject WScript.Shell
    $sc = $shell.CreateShortcut($lnkPath)
    $sc.TargetPath = $targetExe
    $sc.WorkingDirectory = $workDir
    $sc.IconLocation = "$targetExe,0"
    $sc.Description = $desc
    $sc.Save()
  }

  # ---- Platform & Release lookup ------------------------------------------

  if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') {
    $arch = 'arm64'
  } elseif ([Environment]::Is64BitOperatingSystem) {
    $arch = 'amd64'
  } else {
    throw (T 'arch_err' @($app))
  }

  $dir = Join-Path $env:LOCALAPPDATA "Programs\$app"
  $exe = Join-Path $dir "$app.exe"
  $isUpdate = Test-Path $exe

  if ($interactive) {
    Write-Host ''
    Write-StepPending (T 'checking')
  }

  $api = "https://api.github.com/repos/$repo/releases/latest"
  if ($env:GVX_DESKTOP_VERSION) {
    $api = "https://api.github.com/repos/$repo/releases/tags/v$($env:GVX_DESKTOP_VERSION.TrimStart('v'))"
  }
  $release = Invoke-RestMethod -Uri $api -Headers $headers
  $zip = $release.assets | Where-Object { $_.name -like "*-windows-$arch.zip" } | Select-Object -First 1
  if (-not $zip) {
    throw (T 'no_asset' @($release.tag_name, $arch))
  }
  $sum = $release.assets | Where-Object { $_.name -eq "$($zip.name).sha256" } | Select-Object -First 1
  if (-not $sum) {
    throw (T 'no_sum' @($release.tag_name, $zip.name))
  }

  if ($interactive) {
    Write-Banner $release.tag_name $arch $dir $isUpdate
  }

  # ---- 1. Running instance check ------------------------------------------

  $running = Get-RunningApp
  if ($running.Count -gt 0 -and $interactive) {
    $procIds = ($running | ForEach-Object { $_.Id }) -join ', '
    $choice = Invoke-Select `
      -headerFn {
        @(
          @{ Text = ('  ! ' + (T 'running_warn' @($app, $procIds))); Color = [ConsoleColor]::Yellow },
          @{ Text = ('    ' + (T 'running_desc')); Color = [ConsoleColor]::Gray }
        )
      } `
      -itemsFn {
        @(
          (T 'running_close' @($app)),
          (T 'cancel_opt')
        )
      } `
      -defaultIndex 0 `
      -allowYN $true `
      -hintKey 'hint_select'
    Write-Host ''
    if ($choice -ne 0) {
      Write-Host ('  ' + (T 'cancelled')) -ForegroundColor Yellow
      return
    }
  }

  # ---- 2. Installation options (Start Menu + Desktop shortcuts) -----------

  $makeStartMenu = $true
  $makeDesktop = Parse-BoolEnv $env:GVX_DESKTOP_DESKTOP_SHORTCUT $true

  if ($interactive) {
    $opts = Invoke-Checklist `
      -titleFn { T 'options_title' } `
      -labelsFn {
        @(
          (T 'opt_startmenu'),
          (T 'opt_desktop')
        )
      } `
      -defaults @($makeStartMenu, $makeDesktop)
    Write-Host ''
    if ($opts.Cancelled) {
      Write-Host ('  ' + (T 'cancelled')) -ForegroundColor Yellow
      return
    }
    $makeStartMenu = $opts.Checked[0]
    $makeDesktop = $opts.Checked[1]
  }

  # ---- 3. Download, verify, stop if running, install, shortcuts -----------

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("gvx-desktop-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Write-StepPending (T 'step_download' @($app, $release.tag_name, $arch))
    $zipFile = Join-Path $tmp $zip.name
    $sumFile = Join-Path $tmp $sum.name
    Invoke-WebRequest -Uri $zip.browser_download_url -OutFile $zipFile -Headers $headers -UseBasicParsing
    Invoke-WebRequest -Uri $sum.browser_download_url -OutFile $sumFile -Headers $headers -UseBasicParsing
    Write-StepDone (T 'done_download' @($zip.name))

    Write-StepPending (T 'step_verify')
    $want = ((Get-Content -Path $sumFile -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
    $got = (Get-FileHash -Path $zipFile -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($got -ne $want) {
      throw (T 'bad_sum' @($zip.name, $got, $want))
    }
    Write-StepDone (T 'done_verify')

    if ((Get-RunningApp).Count -gt 0) {
      Write-StepPending (T 'step_stop' @($app))
      Stop-RunningApp
      Write-StepDone (T 'done_stop' @($app))
    }

    Write-StepPending (T 'step_extract' @($dir))
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Expand-Archive -Path $zipFile -DestinationPath $dir -Force
    if (-not (Test-Path $exe)) {
      throw (T 'no_exe' @($app))
    }
    Write-StepDone (T 'done_extract' @($dir))

    Write-StepPending (T 'step_shortcuts')
    $desc = T 'shortcut_desc'
    $madeAny = $false
    if ($makeStartMenu) {
      $menu = [Environment]::GetFolderPath('Programs')
      if ($menu) {
        New-AppShortcut (Join-Path $menu "$app.lnk") $exe $dir $desc
        Write-StepDone (T 'done_startmenu')
        $madeAny = $true
      }
    }
    if ($makeDesktop) {
      $desktop = [Environment]::GetFolderPath('Desktop')
      if ($desktop -and (Test-Path $desktop)) {
        New-AppShortcut (Join-Path $desktop "$app.lnk") $exe $dir $desc
        Write-StepDone (T 'done_desktop')
        $madeAny = $true
      }
    }
    if (-not $madeAny) {
      Write-StepDone (T 'done_shortcuts_none')
    }

    Write-Host ''
    Write-Rule ([ConsoleColor]::DarkGreen)
    Write-Host "  $symCheck " -NoNewline -ForegroundColor Green
    Write-Host (T 'complete_title' @($app, $release.tag_name)) -ForegroundColor White
    Write-Rule ([ConsoleColor]::DarkGreen)
    Write-Host ''

    # ---- 4. Launch after install ----------------------------------------

    $shouldLaunch = $false
    if ($env:GVX_DESKTOP_LAUNCH) {
      $shouldLaunch = Parse-BoolEnv $env:GVX_DESKTOP_LAUNCH $false
    } elseif ($interactive) {
      $launchChoice = Invoke-Select `
        -headerFn {
          @(
            @{ Text = ('  ' + (T 'launch_prompt' @($app))); Color = [ConsoleColor]::White }
          )
        } `
        -itemsFn {
          @(
            (T 'launch_yes'),
            (T 'launch_no')
          )
        } `
        -defaultIndex 0 `
        -allowYN $true `
        -hintKey 'hint_confirm'
      Write-Host ''
      $shouldLaunch = ($launchChoice -eq 0)
    }

    if ($shouldLaunch) {
      Write-StepPending (T 'launching' @($app))
      Start-Process -FilePath $exe -WorkingDirectory $dir
    } else {
      Write-Host ('  ' + (T 'run_hint' @($exe))) -ForegroundColor Gray
    }
    Write-Host ''
  } finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}
