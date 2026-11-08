# Install mekiki from GitHub Releases on Windows, for the current user.
#
#   irm https://raw.githubusercontent.com/Mimic890/mekiki/main/install.ps1 | iex
#   $env:VERSION = 'v0.2.2'; irm .../install.ps1 | iex    # a specific version
#
# The archive is checked against the release's SHA256SUMS. Running it again
# replaces the binary. FFmpeg and ExifTool are installed only after asking.

# A function and throw instead of exit: under iex, exit would close the terminal.
function Install-Mekiki {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # the progress bar slows downloads down
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

    $repo = 'Mimic890/mekiki'
    $dir = Join-Path $env:LOCALAPPDATA 'Programs\mekiki'
    $exe = Join-Path $dir 'mekiki.exe'

    $arch = switch ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
        'X64' { 'amd64' }
        'Arm64' { 'arm64' }
        default { throw "unsupported architecture: $_" }
    }
    if ($env:VERSION) {
        $base = "https://github.com/$repo/releases/download/$($env:VERSION)"
    } else {
        $base = "https://github.com/$repo/releases/latest/download"
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $sums = Join-Path $tmp 'SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums
        $line = Get-Content $sums | Where-Object { $_ -match "^[0-9a-f]{64}\s+mekiki_[^/\\]*_windows_$arch\.zip$" } | Select-Object -First 1
        if (-not $line) { throw "no archive for windows/$arch in $base/SHA256SUMS" }
        $want, $archive = -split $line

        Write-Host "Downloading $archive..."
        $zip = Join-Path $tmp $archive
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $zip
        if ((Get-FileHash -Algorithm SHA256 $zip).Hash -ne $want) { throw "checksum mismatch for $archive" }
        Write-Host 'Checksum OK.'

        Expand-Archive -Path $zip -DestinationPath $tmp
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        Copy-Item -Force (Join-Path $tmp "$($archive -replace '\.zip$')\mekiki.exe") $exe
    } finally {
        Remove-Item -Recurse -Force $tmp
    }
    Write-Host "Installed $(& $exe --version) to $exe"

    # The registry value directly: the .NET call would expand %VARS% in the saved PATH.
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    $path = $key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
    if (($path -split ';') -notcontains $dir) {
        $key.SetValue('Path', ($path.TrimEnd(';') + ";$dir").TrimStart(';'), 'ExpandString')
        # Setting any variable tells running programs that the environment changed.
        [Environment]::SetEnvironmentVariable('MEKIKI_INSTALL', '1', 'User')
        [Environment]::SetEnvironmentVariable('MEKIKI_INSTALL', $null, 'User')
        Write-Host "Added $dir to your PATH."
    }

    $ids = @()
    if (@(Get-Command ffmpeg, ffprobe -ErrorAction SilentlyContinue).Count -lt 2) { $ids += 'Gyan.FFmpeg' }
    if (-not (Get-Command exiftool -ErrorAction SilentlyContinue)) { $ids += 'OliverBetz.ExifTool' }
    if ($ids) {
        $commands = $ids | ForEach-Object { "winget install -e --id $_" }
        if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
            Write-Host "FFmpeg or ExifTool is missing and winget was not found. See https://github.com/$repo#install"
        } elseif ([Console]::IsInputRedirected) {
            Write-Host 'FFmpeg or ExifTool is missing. No terminal to ask, so nothing was installed. Run:'
            $commands | ForEach-Object { Write-Host "  $_" }
        } else {
            $reply = Read-Host "FFmpeg or ExifTool is missing. Run `"$($commands -join '; ')`"? [y/N]"
            if ($reply -match '^(y|yes)$') {
                foreach ($id in $ids) {
                    winget install -e --id $id --accept-source-agreements --accept-package-agreements
                    if ($LASTEXITCODE -ne 0) { Write-Host "Could not install $id. Run it yourself: winget install -e --id $id" }
                }
            } else {
                Write-Host "Skipped. To install them later: $($commands -join '; ')"
            }
        }
    }

    # Tools winget just added are in the saved PATH, not yet in this session's.
    $session = $env:Path
    $env:Path = "$([Environment]::GetEnvironmentVariable('Path', 'Machine'));$([Environment]::GetEnvironmentVariable('Path', 'User'));$session"
    & $exe doctor
    $env:Path = $session

    Write-Host ''
    Write-Host 'Done. Open a new terminal to use mekiki.'
}

Install-Mekiki
Remove-Item Function:Install-Mekiki
