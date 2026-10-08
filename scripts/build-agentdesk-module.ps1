param(
 [Parameter(Mandatory=$true)][string]$SourceProject,
 [string]$Out = 'build/agentdesk-module',
 [string]$ModuleVersion = 'remotai-local-1'
)
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$sourceRoot = (Resolve-Path -LiteralPath $SourceProject).Path
$buildRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot $Out))
if (-not $buildRoot.StartsWith($repoRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Module output must remain inside this repository' }
$builderPython = Join-Path $sourceRoot 'build-venv/Scripts/python.exe'
if (-not (Test-Path -LiteralPath $builderPython)) { throw 'AgentDesk build environment not found' }
$snapshotRoot = Join-Path $buildRoot ('source-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $snapshotRoot -Force | Out-Null
# Freeze code inputs while the independent application continues development.
Get-ChildItem -LiteralPath $sourceRoot -File -Filter '*.py' | ForEach-Object { Copy-Item -LiteralPath $_.FullName -Destination $snapshotRoot }
foreach ($asset in @('asr_backends','vocabularies','native','Локальная расшифровка.ico','LocalTranscriber.spec')) {
 Copy-Item -LiteralPath (Join-Path $sourceRoot $asset) -Destination $snapshotRoot -Recurse
}
$adapter = Join-Path $repoRoot 'modules/agentdesk/managed_bridge.py'
Copy-Item -LiteralPath $adapter -Destination $snapshotRoot
$adapter = Join-Path $snapshotRoot 'managed_bridge.py'
$utf8 = [Text.UTF8Encoding]::new($false)
$versionPath = Join-Path $buildRoot 'module-version.json'
$modelHash = (Get-FileHash -LiteralPath (Join-Path $snapshotRoot 'model_catalog.py') -Algorithm SHA256).Hash.ToLowerInvariant()
$adapterHash = (Get-FileHash -LiteralPath $adapter -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText($versionPath, (@{version=$ModuleVersion;model_catalog_sha256=$modelHash;adapter_sha256=$adapterHash} | ConvertTo-Json), $utf8)
$spec = Get-Content -LiteralPath (Join-Path $snapshotRoot 'LocalTranscriber.spec') -Raw
$pythonSource = ($snapshotRoot.Replace('\','/') | ConvertTo-Json -Compress)
$pythonAdapter = ($adapter.Replace('\','/') | ConvertTo-Json -Compress)
$pythonVersion = ($versionPath.Replace('\','/') | ConvertTo-Json -Compress)
$spec = $spec.Replace('project = Path(SPECPATH)', "project = Path($pythonSource)")
$spec = $spec.Replace('str(project / "bridge_main.py")', $pythonAdapter)
$spec = $spec.Replace('("app", "bridge_main")', '("app", "managed_bridge")')
$spec = $spec.Replace('entry[0] == "bridge_main"', 'entry[0] == "managed_bridge"')
$spec = $spec.Replace('a = Analysis(', "datas += [($pythonVersion, `".`")]`n`na = Analysis(")
$specPath = Join-Path $buildRoot 'RemotaiAgentDesk.spec'
[IO.File]::WriteAllText($specPath, $spec, $utf8)
$distRoot = Join-Path $buildRoot 'dist'
$workRoot = Join-Path $buildRoot 'pyinstaller'
& $builderPython -m PyInstaller --noconfirm --distpath $distRoot --workpath $workRoot $specPath
if ($LASTEXITCODE -ne 0) { throw 'Managed module build failed' }
Write-Output (Join-Path $distRoot 'Панель агента/AgentDeskBridge.exe')
