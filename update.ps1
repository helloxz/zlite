# zlite Windows self-updater: updates an existing zlite installation to the
# latest release, keeping the upgrade-friendly layout.
#
# Design notes:
#   - Requires an existing installation: entry detection order is
#     -Dir -> PATH (Get-Command, external programs only) -> $HOME\.zlite\bin\zlite.exe.
#     When nothing is found it prints an install hint and exits.
#   - Layout detection: install.ps1 uses a plain COPY for the command entry
#     (no symlink - Windows symlinks need admin rights or developer mode), so
#     "versioned layout" is detected by the presence of zlite-<version>.exe
#     next to the entry.
#   - Local version: the newest zlite-<version>.exe filename is preferred (works
#     even if the binary is broken), else `zlite --version`.
#   - Version guard: never downgrade unless -Version / -Force is given.
#   - Windows-specific: a running zlite.exe cannot be replaced (file lock), so
#     the updater refuses to run while a zlite process is active.
#   - Safety: the versioned binary is fully placed and VERIFIED before the
#     entry is replaced, so a broken download never breaks the current installation.
#   - Runtime state (config under $ZLITE_DATA, default ~/.zlite) is never
#     touched by an update; only binaries under the bin dir are replaced.
#   - Output is English for broad compatibility.
#
# Usage (PowerShell; runs in memory, no ExecutionPolicy change needed):
#   irm https://raw.githubusercontent.com/helloxz/zlite/main/update.ps1 | iex
# Or download and run with arguments (needs -ExecutionPolicy Bypass):
#   powershell -ExecutionPolicy Bypass -File update.ps1 [-Version 0.1.0] [-Force]
#
# Environment variables: ZLITE_VERSION, ZLITE_REPO, ZLITE_BASE_URL,
# ZLITE_API_BASE, ZLITE_DIR, ZLITE_FORCE
param(
  [string]$Version = $env:ZLITE_VERSION,
  [switch]$Force,
  [string]$Dir = $env:ZLITE_DIR,
  [string]$Repo = $env:ZLITE_REPO,
  [string]$BaseUrl = $env:ZLITE_BASE_URL,
  [string]$ApiBase = $env:ZLITE_API_BASE,
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

# --- Execution mode ---
$script:isInMemory = [string]::IsNullOrEmpty($PSScriptRoot)

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
$force = $Force -or ($env:ZLITE_FORCE -eq '1')

if ($Help) {
  Write-Host @'
zlite updater (Windows)

Usage:
  irm https://raw.githubusercontent.com/helloxz/zlite/main/update.ps1 | iex
  powershell -ExecutionPolicy Bypass -File update.ps1 [options]

Options:
  -Version <ver>      Update to a specific version (v prefix optional); default: latest
  -Force              Update even when the local version is already the latest
  -Dir <dir>          Install directory (contains zlite.exe and zlite-<ver>.exe);
                      default: detected from the existing install
  -Repo <owner/repo>  Override repository (default: helloxz/zlite)
  -BaseUrl <url>      Download URL prefix (e.g. a mirror)
  -ApiBase <url>      GitHub API URL (for resolving latest)

Environment variables: ZLITE_VERSION, ZLITE_REPO, ZLITE_BASE_URL,
  ZLITE_API_BASE, ZLITE_DIR, ZLITE_FORCE

Install layout (managed by install.ps1; update.ps1 keeps it):
  <dir>\zlite.exe            command entry (copy of the newest version)
  <dir>\zlite-<version>.exe  versioned binary
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

function Test-NewerVersion {
  param([string]$A, [string]$B)
  $a = ConvertTo-VersionNumber $A
  $b = ConvertTo-VersionNumber $B
  if ($a -and $b) { return $a -gt $b }
  return ([string]::Compare($A, $B, [StringComparison]::Ordinal) -gt 0)
}

# --- Locate the installed entry ---
function Find-Entry {
  if ($Dir) {
    $p = Join-Path $Dir 'zlite.exe'
    if (Test-Path -LiteralPath $p) { return $p }
    throw "error: no zlite.exe found in -Dir '$Dir'"
  }
  $cmd = Get-Command -Name zlite -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($cmd) { return $cmd.Source }
  $def = Join-Path $HOME '.zlite\bin\zlite.exe'
  if (Test-Path -LiteralPath $def) { return $def }
  return $null
}

# --- Newest versioned binary in the bin dir ---
function Find-NewestVersioned {
  param([string]$BinDir)
  $files = @(Get-ChildItem -Path $BinDir -Filter 'zlite-*.exe' -File -ErrorAction SilentlyContinue)
  # Only real zlite-<x.y.z>.exe names count
  $versioned = @($files) | Where-Object {
    (($_.Name -replace '^zlite-', '') -replace '\.exe$', '') -match '^\d+(\.\d+){1,3}'
  }
  if (-not $versioned) { return $null }
  return $versioned | Sort-Object -Property {
    ConvertTo-VersionNumber (($_.Name -replace '^zlite-', '') -replace '\.exe$', '')
  } -Descending | Select-Object -First 1
}

# --- Local version: newest versioned filename first, else `zlite --version` ---
function Get-LocalVersion {
  param([string]$Entry)
  $binDir = Split-Path -Parent $Entry
  $newest = Find-NewestVersioned -BinDir $binDir
  if ($newest) {
    return (($newest.Name -replace '^zlite-', '') -replace '\.exe$', '')
  }
  $out = @(& $Entry --version 2>$null)
  if (-not $out) { return $null }
  $line = [string]($out | Select-Object -First 1)
  $parts = $line -split '\s+'
  if ($parts.Count -lt 2) { return $null }
  return ([string]$parts[1]) -replace '^v', ''
}

# --- Main flow ---
function Update-Zlite {
  param([string]$Arch)

  # --- 1. Require an existing installation ---
  $entry = Find-Entry
  if (-not $entry) {
    throw "zlite is not installed. Install it first:`n  irm https://raw.githubusercontent.com/$Repo/main/install.ps1 | iex"
  }
  Write-Host "==> Found zlite: $entry"
  $binDir = Split-Path -Parent $entry

  # --- Windows-specific: refuse to run while zlite is active ---
  $proc = Get-Process -Name zlite -ErrorAction SilentlyContinue
  if ($proc) {
    $ids = @($proc.Id) -join ', '
    throw "error: zlite is currently running (PID $ids). Stop it first, then re-run the updater - Windows cannot replace an in-use executable"
  }

  # --- 2. Version check (skipped when a specific version is given or --force) ---
  $tag = ''
  $url = ''
  if (-not $Version -and -not $force) {
    $localVer = Get-LocalVersion -Entry $entry
    if ($localVer) {
      Write-Host "==> Local version: $localVer"
      $info = Get-ReleaseInfo -Arch $Arch
      $tag = $info.Tag
      $url = $info.Url
      $remoteVer = $tag -replace '^v', ''
      Write-Host "==> Remote version: $remoteVer"
      if ($localVer -eq $remoteVer) {
        Write-Host "==> Already up to date (v$localVer); nothing to do (use -Force to reinstall)"
        return
      }
      if (Test-NewerVersion -A $localVer -B $remoteVer) {
        Write-Host "==> Local version ($localVer) is newer than the remote ($remoteVer); nothing to do (use -Version / -Force to downgrade)"
        return
      }
      Write-Host "==> Updating $localVer -> $remoteVer..."
    } else {
      Write-Host '==> Could not determine the local version; updating anyway'
      $info = Get-ReleaseInfo -Arch $Arch
      $tag = $info.Tag
      $url = $info.Url
    }
  }

  # --- Determine tag and download URL ---
  if (-not $tag) {
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
  }

  # --- 3. Download to a temp dir ---
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('zlite-update-' + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
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

    # --- 4. Place the versioned binary, verify it, then replace the entry ---
    New-Item -ItemType Directory -Path $binDir -Force | Out-Null
    $ver = $tag -replace '^v', ''
    $target = Join-Path $binDir "zlite-$ver.exe"
    try {
      Copy-Item -Path $bin.FullName -Destination $target -Force
    } catch {
      throw "error: cannot write to $binDir (is it read-only? try running PowerShell as Administrator): $_"
    }
    Write-Host "==> Installed: $target"
    Write-Host '==> Verifying the new binary...'
    $null = & $target --version
    if ($LASTEXITCODE -ne 0) { throw "'zlite --version' on the new binary failed with exit code $LASTEXITCODE" }

    $entryCopy = Join-Path $binDir 'zlite.exe'
    try {
      Copy-Item -Path $bin.FullName -Destination $entryCopy -Force
    } catch {
      throw "error: cannot replace the command entry $entryCopy (is it read-only or still running?): $_"
    }
    Write-Host "==> Replaced: $entryCopy"

    # --- 5. Verify (through the command entry) BEFORE pruning ---
    Write-Host '==> Verifying...'
    & $entryCopy --version
    if ($LASTEXITCODE -ne 0) { throw "'zlite --version' failed with exit code $LASTEXITCODE" }

    # --- 6. Prune old versions: keep the current plus the newest previous one ---
    $oldVersions = Get-ChildItem -Path $binDir -Filter 'zlite-*.exe' -File |
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

    # --- PATH hint ---
    $norm = $binDir.TrimEnd('\')
    $onPath = @($env:Path -split ';') | Where-Object { $_ -and $_.TrimEnd('\') -ieq $norm }
    if (-not $onPath) {
      Write-Host ''
      Write-Host "hint: $binDir is not on your PATH. To add it, run:"
      Write-Host "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';$binDir', 'User')"
      Write-Host '  (or via: Settings > System > About > Advanced system settings > Environment Variables)'
    }

    Write-Host ''
    Write-Host 'Update complete. The previous version is kept for rollback.'
  } finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# --- Entry ---
try {
  $arch = Get-Arch
  Write-Host "==> Target platform: windows/$arch"
  Update-Zlite -Arch $arch
} catch {
  Write-Host "error: $_" -ForegroundColor Red
  if ($script:isInMemory) { return }
  exit 1
}
