Param(
    [string]$Configuration = "Debug",
    [string]$Platform = "x64",
    [switch]$SkipNative,
    [switch]$SkipGo,
    [switch]$SkipImportGen,
    [switch]$Verbose,
    [string]$MSBuildPath,
    [string]$ExamplePath
)

$ErrorActionPreference = 'Stop'

function Log($msg) { if ($Verbose) { Write-Host "[build] $msg" } }

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$nativeSln = Join-Path $repoRoot 'native/WinUI3Native/WinUI3Native.sln'
$binDir   = Join-Path $repoRoot "bin/$Platform/$Configuration"

function Resolve-MSBuild {
    param([string]$Explicit)
    if ($Explicit -and (Test-Path $Explicit)) { return (Resolve-Path $Explicit).Path }
    $probe = Get-Command msbuild -ErrorAction SilentlyContinue
    if ($probe) { return $probe.Path }
    # Try vswhere
    $vsWhere = "$Env:ProgramFiles(x86)\Microsoft Visual Studio\Installer\vswhere.exe"
    if (Test-Path $vsWhere) {
        $vsInstall = & $vsWhere -latest -products * -requires Microsoft.Component.MSBuild -property installationPath 2>$null
        if ($LASTEXITCODE -eq 0 -and $vsInstall) {
            $candidate = Join-Path $vsInstall 'MSBuild/Current/Bin/MSBuild.exe'
            if (Test-Path $candidate) { return $candidate }
        }
    }
    # Common 2022 Community default path fallback
    $common = "$Env:ProgramFiles\Microsoft Visual Studio\2022\Community\MSBuild\Current\Bin\MSBuild.exe"
    if (Test-Path $common) { return $common }
    return $null
}

$msbuildExe = Resolve-MSBuild -Explicit $MSBuildPath
if (-not $msbuildExe -and -not $SkipNative) {
    Write-Warning "Could not locate MSBuild. Native build will be skipped. Provide -MSBuildPath or install Build Tools."
    $SkipNative = $true
}

if (-not (Test-Path $binDir)) { New-Item -ItemType Directory -Force -Path $binDir | Out-Null }

if (-not $SkipNative) {
    if (Test-Path $nativeSln) {
        Log "Cleaning native solution ($Configuration|$Platform)"
        & $msbuildExe "$nativeSln" /t:Clean /p:Configuration=$Configuration /p:Platform=$Platform 2>$null | Out-Null
        Log "Building native solution ($Configuration|$Platform) using: $msbuildExe"
        & $msbuildExe "$nativeSln" /p:Configuration=$Configuration /p:Platform=$Platform /restore
        if ($LASTEXITCODE -ne 0) { throw "MSBuild failed with exit code $LASTEXITCODE" }
    } else {
        Write-Warning "Native solution not found at $nativeSln (skipping native build)."
    }
}

# Locate native output artifacts (DLL, LIB, PDB, PRI, WINMD) even if VS produced nested directories.
$nativeArtifacts = @()
if (-not $SkipNative) {
    $searchRoot = Join-Path $repoRoot 'native/WinUI3Native'
    $patterns = @('WinUI3Native.dll','WinUI3Native.lib','WinUI3Native.pdb','WinUI3Native.pri','WinUI3Native.winmd')
    foreach ($pat in $patterns) {
        $found = Get-ChildItem -Path $searchRoot -Recurse -Filter $pat -File -ErrorAction SilentlyContinue | Where-Object { $_.FullName -match "\\$Platform\\$Configuration\\" }
        if (-not $found) { $found = Get-ChildItem -Path $searchRoot -Recurse -Filter $pat -File -ErrorAction SilentlyContinue | Select-Object -First 1 }
        if ($found) { $nativeArtifacts += $found }
    }
    # Copy the explicitly tracked artifacts
    foreach ($f in $nativeArtifacts) {
        try {
            Copy-Item $f.FullName $binDir -Force
            Log "Copied native artifact -> $binDir\\$($f.Name)"
        } catch {
            Write-Warning "Skipping copy of $($f.Name): $($_.Exception.Message)"
        }
    }
    if ($nativeArtifacts.Count -eq 0) {
        Write-Warning "No native artifacts discovered for copy step."
    } else {
        # Discover directory of primary DLL and copy all sibling DLLs (dependencies)
        $primary = $nativeArtifacts | Where-Object { $_.Name -ieq 'WinUI3Native.dll' } | Select-Object -First 1
        if ($primary) {
            $dllDir = Split-Path $primary.FullName -Parent
            $siblingDlls = Get-ChildItem -Path $dllDir -Filter *.dll -File -ErrorAction SilentlyContinue
            foreach ($sd in $siblingDlls) {
                if ($sd.Name -notin ($nativeArtifacts | ForEach-Object { $_.Name })) {
                    try {
                        Copy-Item $sd.FullName $binDir -Force
                        Log "Copied dependency DLL -> $binDir\\$($sd.Name)"
                    } catch {
                        Write-Warning "Skipping copy of dependency $($sd.Name): $($_.Exception.Message)"
                    }
                }
            }
        }
    }
}

<#
    Import library generation & copy removal rationale:
    The Go wrapper no longer embeds LDFLAGS. We now set CGO_LDFLAGS to point
    at the bin output directory so consumers can switch configurations
    (Debug/Release, x64/arm64) without touching source. We still optionally
    allow creation of a MinGW-style .a for downstream tooling, but we do NOT
    copy it into internal/winui (keeping the wrapper pure).
#>

# Optionally generate MinGW-style import lib (.a) from produced DLL using gendef + dlltool (if available)
if (-not $SkipImportGen) {
    $dllName = 'WinUI3Native.dll'
    $dllPath = Join-Path $binDir $dllName
    if (Test-Path $dllPath) {
        $gendef = Get-Command gendef -ErrorAction SilentlyContinue
        $dlltool = Get-Command dlltool -ErrorAction SilentlyContinue
        if ($gendef -and $dlltool) {
            Log "Generating .def from $dllName"
            Push-Location $binDir
            & $gendef.Path $dllName | Out-Null
            $defFile = Join-Path $binDir ($dllName -replace '.dll$','.def')
            if (Test-Path $defFile) {
                $aOut = 'libWinUI3Native.a'
                Log "Creating $aOut via dlltool"
                & $dlltool.Path -d (Split-Path -Leaf $defFile) -l $aOut -D $dllName
                if (Test-Path (Join-Path $binDir $aOut)) {
                    Log "Generated $aOut (kept in bin only)"
                } else {
                    Write-Warning "dlltool did not produce $aOut"
                }
            } else {
                Write-Warning "gendef did not produce expected .def file"
            }
            Pop-Location
        } else {
            Write-Warning "Skipping import lib generation: gendef or dlltool not found in PATH."
        }
    } else {
        Write-Warning "DLL not found at $dllPath; cannot generate .a import lib."
    }
}

if (-not $SkipGo) {
    # Require an example path pointing to main.go OR a directory containing main.go
    if (-not $ExamplePath) {
        throw "Missing -ExamplePath. Provide a path to a main.go file or a directory containing main.go (e.g., examples\\yourgoapp or cmd\\yourgoapp\\main.go)."
    }

    # Resolve to absolute path against repo root when relative
    $candidate = $ExamplePath
    if (-not [System.IO.Path]::IsPathRooted($candidate)) {
        $candidate = Join-Path $repoRoot $candidate
    }
    if (-not (Test-Path -LiteralPath $candidate)) {
        throw "Example path not found: $ExamplePath"
    }

    # Determine package directory to build
    $pkgDir = $null
    $item = Get-Item -LiteralPath $candidate
    if ($item.PSIsContainer) {
        $pkgDir = $item.FullName
        $mainFile = Join-Path $pkgDir 'main.go'
        if (-not (Test-Path -LiteralPath $mainFile)) {
            throw "Directory '$ExamplePath' does not contain a main.go file."
        }
    } else {
        # Must be a main.go file
        if ($item.Extension -ne '.go' -or $item.Name -ne 'main.go') {
            throw "-ExamplePath must be a directory containing main.go or the main.go file itself. Got: '$ExamplePath'"
        }
        $pkgDir = $item.Directory.FullName
    }

    Log "Building Go executable from '$pkgDir'"
    Push-Location $repoRoot
    # (Legacy) Set CGO vars for optional future cgo variants. Current wrapper uses dynamic syscall loading.
    $env:CGO_ENABLED = 0
    $env:CGO_LDFLAGS = "-L$binDir -lWinUI3Native -lole32 -luser32"  # harmless if CGO disabled
    # For Debug we keep a console for live logs; for Release we hide it.
    if ($Configuration -ieq 'Debug') {
        $outputExe = Join-Path $binDir 'debug.exe'
        $env:WINUI_DEBUG = '1'
        go build -o $outputExe $pkgDir
        Log "Built Debug (console visible, WINUI_DEBUG=1)."
    } else {
        $outputExe = Join-Path $binDir 'release.exe'
        go build -ldflags "-H windowsgui" -o $outputExe $pkgDir
        Log "Built Release (GUI subsystem, no console)."
    }
    Pop-Location
    Log "Executable and all required DLLs reside in $binDir"
}

Write-Host "Build complete. Artifacts in $binDir"
