# Install the `scout` command from a GitHub release (Windows).
#   irm https://raw.githubusercontent.com/sshuvoo/scout-claw/main/install.ps1 | iex
# Env: SCOUT_VERSION (default: latest), SCOUT_INSTALL_DIR (default: ~\.local\bin),
#      SCOUT_NO_MODIFY_PATH=1 to leave the user PATH unchanged.
# Errors throw instead of calling exit, which would close the caller's shell under iex.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar makes downloads very slow on PowerShell 5.1
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'sshuvoo/scout-claw'
$Dir = if ($env:SCOUT_INSTALL_DIR) { $env:SCOUT_INSTALL_DIR } else { Join-Path $HOME '.local\bin' }

# PROCESSOR_ARCHITEW6432 is set when a 32-bit PowerShell runs on a 64-bit OS.
$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
  'AMD64' { $arch = 'amd64' }
  'ARM64' { $arch = 'arm64' }
  default { throw "unsupported architecture: $cpu" }
}

$tag = $env:SCOUT_VERSION
if (-not $tag) {
  $tag = (Invoke-RestMethod -UseBasicParsing -Headers @{ 'User-Agent' = 'scout-installer' } `
      -Uri "https://api.github.com/repos/$Repo/releases/latest").tag_name
}
if (-not $tag) { throw 'could not determine latest release' }
if (-not $tag.StartsWith('v')) { $tag = "v$tag" }

$ver = $tag.Substring(1)
$name = "scoutclaw_${ver}_windows_${arch}.zip"
$base = "https://github.com/$Repo/releases/download/$tag"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("scout-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "downloading $name ($tag)"
  $zip = Join-Path $tmp $name
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $zip
  Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')
  $want = Get-Content (Join-Path $tmp 'checksums.txt') |
    ForEach-Object { $f = $_ -split '\s+'; if ($f[1] -eq $name) { $f[0] } } |
    Select-Object -First 1
  $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash
  if (-not $want -or $want -ne $got) { throw 'checksum mismatch, aborting' } # -ne ignores case

  Expand-Archive -Path $zip -DestinationPath (Join-Path $tmp 'x') -Force
  $exe = Join-Path $tmp 'x\scout.exe'
  if (-not (Test-Path $exe)) { throw 'archive does not contain scout.exe' }

  New-Item -ItemType Directory -Force -Path $Dir | Out-Null
  $dest = Join-Path $Dir 'scout.exe'
  if (Test-Path $dest) {
    # A running scout.exe cannot be overwritten, but it can be renamed.
    Remove-Item "$dest.old" -Force -ErrorAction SilentlyContinue
    Move-Item $dest "$dest.old" -Force
  }
  Copy-Item $exe $dest
  Write-Host "installed $dest"
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$parts = @(($userPath -split ';') | Where-Object { $_ })
if (($parts | ForEach-Object { $_.TrimEnd('\') }) -notcontains $Dir.TrimEnd('\')) {
  if ($env:SCOUT_NO_MODIFY_PATH) {
    Write-Host "add $Dir to your PATH to run scout from anywhere"
  } else {
    [Environment]::SetEnvironmentVariable('Path', (($parts + $Dir) -join ';'), 'User')
    $env:Path = "$env:Path;$Dir"
    Write-Host "added $Dir to your user PATH (open a new terminal for other windows to see it)"
  }
}
Write-Host 'next: run  scout setup  (stores your OpenAI API key), then  scout doctor --online'
Write-Host 'tip: install Git for Windows (https://git-scm.com/downloads/win) so scout can run bash scripts too'
