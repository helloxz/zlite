# zlite Windows one-click installer: auto-detects the CPU architecture,
# downloads the matching release package (zip) from GitHub Releases and
# installs it into a user directory.
#
# Design notes:
#   - Architecture detection: PROCESSOR_ARCHITECTURE, with
#     PROCESSOR_ARCHITEW6432 for a 32-bit PowerShell running on 64-bit Windows.
#   - Version policy: the latest release is installed by default (resolved
#     via the GitHub API); -Version pins a specific version.
#   - Package format: windows always downloads the .zip asset.
#   - Upgrade-friendly layout:
#       $HOME\.zlite\bin\zlite.exe            -> command entry (copy of the newest version)
#       $HOME\.zlite\bin\zlite-<version>.exe  -> versioned binary
#     The entry is a plain copy, NOT a symlink: Windows symlinks require
#     admin rights or developer mode. The previous version is kept for rollback;
#     older ones are pruned.
#   - PATH: the bin dir is added to the *user* PATH (HKCU\Environment) via
#     the registry API with ExpandString (preserves %VAR% entries), not setx.
#   - TLS 1.2 is forced up front: PowerShell 5.1 on older Windows defaults
#     to TLS 1.0, which GitHub rejects.
#   - Output is English for broad compatibility.
#
# Usage (PowerShell; runs in memory, no ExecutionPolicy change needed):
#   irm https://raw.githubusercontent.com/helloxz/zlite/main/install.ps1 | iex
# Or download and run with arguments (needs -ExecutionPolicy Bypass):
#   powershell -ExecutionPolicy Bypass -File install.ps1 [-Version 0.1.0] [-NoPath]
#
# Environment variables (equivalent to the parameters; the only channel when
# using irm | iex): ZLITE_VERSION, ZLITE_REPO, ZLITE_BASE_URL, ZLITE_API_BASE,
# ZLITE_BIN_DIR, ZLITE_NO_PATH
param(
  [string]$Version = $env:ZLITE_VERSION,
  [string]$Repo = $env:ZLITE_REPO,
  [string]$BaseUrl = $env:ZLITE_BASE_URL,
  [string]$ApiBase = $env:ZLITE_API_BASE,
  [string]$BinDir = $env:ZLITE_BIN_DIR,
  [switch]$NoPath,
  [switch]$Help
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# --- Force TLS 1.2 up front ---
try {
  [Net.ServicePointManager]::SecurityProtocol = `
    [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {
}

# --- Execution mode: irm | iex runs in memory where 'exit' would close the
#     user's whole PowerShell session, so every exit point uses return/exit ---
$script:isInMemory = [string]::IsNullOrEmpty($PSScriptRoot)
$script:isDotSourced = $MyInvocation.InvocationName -eq '.'

# --- Basic env check ---
if ($PSVersionTable.PSVersion.Major -lt 5) {
  Write-Host "error: PowerShell 5.0 or newer is required (current: $($PSVersionTable.PSVersion))" -ForegroundColor Red
  if ($script:isInMemory) { return }
  exit 1
}

# --- Defaults ---
if (-not $Repo) { $Repo = 'helloxz/zlite' }
if (-not $BaseUrl) { $BaseUrl = 'https://github.com' }
if (-not $ApiBase) { $ApiBase = 'https://api.github.com' }
$skipPath = $NoPath -or ($env:ZLITE_NO_PATH -eq '1')

if ($Help) {
  Write-Host @'
zlite installer (Windows)

Usage:
  irm https://raw.githubusercontent.com/helloxz/zlite/main/install.ps1 | iex
  powershell -ExecutionPolicy Bypass -File install.ps1 [options]

Options:
  -Version <ver>      Install a specific version (v prefix optional); default: latest
  -Repo <owner/repo>  Override repository (default: helloxz/zlite)
  -BaseUrl <url>      Download URL prefix (e.g. a mirror)
  -ApiBase <url>      GitHub API URL (for resolving latest)
  -BinDir <dir>       Binary directory (default: $HOME\.zlite\bin)
  -NoPath             Do not modify the user PATH

Environment variables: ZLITE_VERSION, ZLITE_REPO, ZLITE_BASE_URL,
  ZLITE_API_BASE, ZLITE_BIN_DIR, ZLITE_NO_PATH

Install layout (upgrade-friendly):
  $HOME\.zlite\bin\zlite.exe            command entry (copy of the newest version)
  $HOME\.zlite\bin\zlite-<version>.exe  versioned binary
  The previous version is kept for rollback; older ones are pruned.
'@
  if ($script:isInMemory) { return }
  exit 0
}

# --- Architecture detection ---
function Get-Arch {
  $arch = $env:PROCESSOR_ARCHITEW6432
  if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
  switch ($arch) {
    'AMD64' { return 'amd64' }
    'ARM64' { return 'arm64' }
    default { throw "error: unsupported CPU architecture '$arch' (zlite release packages are only provided for amd64 / arm64)" }
  }
}

# --- Resolve the latest release, returns @{Tag; Url} ---
function Get-ReleaseInfo {
  param([string]$Arch)
  $headers = @{ 'User-Agent' = 'zlite-installer' }
  $apiUrl = "$ApiBase/repos/$Repo/releases/latest"
  Write-Host "==> Resolving latest release: $apiUrl"
  $release = $null
  try {
    $release = Invoke-RestMethod -Uri $apiUrl -Headers $headers
  } catch {
    Write-Host '==> No stable release yet, falling back to the newest release (including prereleases)...'
    try {
      $list = Invoke-RestMethod -Uri "$ApiBase/repos/$Repo/releases?per_page=1" -Headers $headers
      $release = $list[0]
    } catch {
      throw 'error: failed to fetch the release list (network issue or API rate limit?)'
    }
  }
  if (-not $release) { throw 'error: empty response from the GitHub API' }
  $tag = [string]$release.tag_name
  if (-not $tag) { throw 'error: no tag_name found in the latest release response' }
  $asset = @($release.assets) | Where-Object { $_.name -like "zlite-v*-windows-$Arch.zip" } | Select-Object -First 1
  if (-not $asset) { throw "error: no windows/$Arch asset found in the latest release (missing package or unexpected API response)" }
  return @{ Tag = $tag; Url = [string]$asset.browser_download_url }
}

# --- Tag normalization: 0.1.0 -> v0.1.0 ---
function Get-Tag {
  param([string]$V)
  if ($V -like 'v*') { return $V }
  return "v$V"
}

# --- Version helpers ---
function ConvertTo-VersionNumber {
  param([string]$V)
  $v = $V -replace '-.*$', ''
  if ($v -match '^\d+(\.\d+){1,3}$') { return [version]$v }
  return $null
}

# --- Main flow ---
function Install-Zlite {
  param([string]$Arch)

  # --- Determine tag and download URL ---
  $tag = ''
  $url = ''
  if (-not $Version -or $Version -eq 'latest') {
    $info = Get-ReleaseInfo -Arch $Arch
    $tag = $info.Tag
    $url = $info.Url
  } else {
    $tag = Get-Tag $Version
    $file = "zlite-$tag-windows-$Arch.zip"
    $url = "$BaseUrl/$Repo/releases/download/$tag/$file"
    Write-Host "==> Version: $tag"
  }

  # --- Temp dir ---
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('zlite-install-' + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    # --- Download ---
    $pkg = Join-Path $tmp 'pkg.zip'
    Write-Host "==> Downloading: $url"
    try {
      Invoke-WebRequest -Uri $url -OutFile $pkg -UseBasicParsing -Headers @{ 'User-Agent' = 'zlite-installer' }
    } catch {
      throw "error: download failed ($url). Please check: 1) the version/repo name is correct; 2) a windows/$Arch release package exists; 3) GitHub is reachable from your network"
    }
    $pkgLen = (Get-Item $pkg).Length
    if ($pkgLen -eq 0) { throw "error: download failed or the file is empty ($url)" }
    Write-Host "==> Downloaded: $pkgLen bytes"

    # --- Extract and locate the binary ---
    $unpacked = Join-Path $tmp 'unpacked'
    Expand-Archive -Path $pkg -DestinationPath $unpacked
    $bin = Get-ChildItem -Path $unpacked -Recurse -Filter 'zlite.exe' -File | Select-Object -First 1
    if (-not $bin) { throw 'error: no zlite.exe found in the release package' }

    # --- Binary dir (default ~/.zlite/bin) ---
    if (-not $BinDir) { $BinDir = Join-Path $HOME '.zlite\bin' }
    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null

    # --- Versioned binary (strip the v prefix to match 'zlite --version') ---
    $ver = $tag -replace '^v', ''
    $target = Join-Path $BinDir "zlite-$ver.exe"
    Copy-Item -Path $bin.FullName -Destination $target -Force
    Write-Host "==> Installed: $target"

    # --- Command entry: copy the newest version as zlite.exe ---
    $entry = Join-Path $BinDir 'zlite.exe'
    Copy-Item -Path $bin.FullName -Destination $entry -Force
    Write-Host "==> Installed: $entry"

    # --- Verify (through the command entry) BEFORE pruning ---
    Write-Host '==> Verifying...'
    & $entry --version
    if ($LASTEXITCODE -ne 0) { throw "'zlite --version' failed with exit code $LASTEXITCODE" }

    # --- Prune old versions: keep the current plus the newest previous one ---
    $oldVersions = Get-ChildItem -Path $BinDir -Filter 'zlite-*.exe' -File |
      Where-Object { $_.FullName -ne $target } |
      Sort-Object -Property {
        $v = (($_.Name -replace '^zlite-', '') -replace '\.exe$', '') -replace '-.*$', ''
        if ($v -match '^\d+(\.\d+){1,3}$') { [version]$v } else { [version]'0.0.0' }
      } -Descending
    $keptPrev = $false
    foreach ($f in $oldVersions) {
      if (-not $keptPrev) {
        Write-Host "==> Keeping previous version: $($f.FullName)"
        $keptPrev = $true
        continue
      }
      Remove-Item -Path $f.FullName -Force
      Write-Host "==> Removed old version: $($f.FullName)"
    }

    # --- PATH: add the bin dir to the user PATH (registry API) ---
    $norm = $BinDir.TrimEnd('\')
    if (-not $skipPath) {
      $reg = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
      if (-not $reg) { throw 'error: cannot open HKCU\Environment for writing' }
      try {
        $userPath = [string]$reg.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        try { $kind = $reg.GetValueKind('Path') } catch { $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString }
        $exists = @($userPath -split ';') | Where-Object { $_ -and $_.TrimEnd('\') -ieq $norm }
        if ($exists) {
          Write-Host "==> Already on user PATH: $BinDir"
        } else {
          $newPath = if ([string]::IsNullOrEmpty($userPath)) { $norm } else { $userPath.TrimEnd(';') + ';' + $norm }
          $reg.SetValue('Path', $newPath, $kind)
          Write-Host "==> Added to user PATH: $BinDir"
        }
      } finally {
        $reg.Dispose()
      }

      # Update current process PATH
      $currentHasBin = @($env:Path -split ';') | Where-Object { $_ -and $_.TrimEnd('\') -ieq $norm }
      if (-not $currentHasBin) {
        $env:Path = $norm + ';' + $env:Path
      }

      # Broadcast WM_SETTINGCHANGE (best-effort)
      try {
        if (-not ('Zlite.Native.EnvNotify' -as [type])) {
          $envNotifySource = @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
          Add-Type -Namespace Zlite.Native -Name EnvNotify -MemberDefinition $envNotifySource
        }
        $result = [UIntPtr]::Zero
        $null = [Zlite.Native.EnvNotify]::SendMessageTimeout(
          [IntPtr]0xffff, 0x1a, [UIntPtr]::Zero, 'Environment', 0x0002, 5000, [ref]$result)
      } catch {
        Write-Host '==> Note: environment-change notification skipped (harmless)'
      }
    }

    # --- Completion hints ---
    Write-Host ''
    Write-Host 'Installation complete. Run zlite to start.'
    Write-Host 'To upgrade later, re-run this script (the previous version is kept for rollback).'
    if (-not $skipPath) {
      if ($script:isInMemory -or $script:isDotSourced) {
        Write-Host 'Start it (available in this terminal right away) with: zlite'
      } else {
        Write-Host 'Start it (in a new terminal) with: zlite'
        Write-Host 'Or make this terminal work right away, run:'
        Write-Host "  `$env:Path = `"$norm;`$env:Path`""
      }
    } else {
      Write-Host "Start it with: & `"$entry`""
    }
  } finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# --- Entry ---
try {
  $arch = Get-Arch
  Write-Host "==> Target platform: windows/$arch"
  Install-Zlite -Arch $arch
} catch {
  Write-Host "error: $_" -ForegroundColor Red
  if ($script:isInMemory) { return }
  exit 1
}
