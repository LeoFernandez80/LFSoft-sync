<#
.SYNOPSIS
    Construye el paquete de deploy del servicio LFSoft-sync.
.DESCRIPTION
    Compila el binario Go y arma la carpeta deploy\ lista para copiar al servidor.
    Ejecutar desde la raiz del proyecto LFSoft-sync.
.EXAMPLE
    .\scripts\deploy-build.ps1
    .\scripts\deploy-build.ps1 -PgDSN "postgres://user:pass@10.0.0.5:5432/lfsoft_hidramic?sslmode=disable"
#>
param(
    [string]$PgDSN = "postgres://postgres:lfsoft@localhost:5432/lfsoft_hidramic?sslmode=disable",
    [string]$MdbBasePath = "C:\PSB\Hidramic",
    [int]$SyncPort = 9090,
    [string]$OutputDir = "deploy"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if (-not (Test-Path "$root\LFSoft-sync")) {
    $root = Split-Path -Parent $PSScriptRoot
}
if (-not (Test-Path "$root\cmd\sync\main.go")) {
    $root = "C:\Desarrollo\LFSoft-sync"
}

Write-Host "=== LFSoft-sync: Build de deploy ===" -ForegroundColor Cyan
Write-Host "Raiz del proyecto: $root"

$deploy = Join-Path $root $OutputDir
if (Test-Path $deploy) {
    Remove-Item -Recurse -Force $deploy
}

New-Item -ItemType Directory -Force $deploy | Out-Null
New-Item -ItemType Directory -Force "$deploy\config\entities" | Out-Null
New-Item -ItemType Directory -Force "$deploy\migrations" | Out-Null
New-Item -ItemType Directory -Force "$deploy\migrations-app" | Out-Null
New-Item -ItemType Directory -Force "$deploy\logs" | Out-Null
New-Item -ItemType Directory -Force "$deploy\tools" | Out-Null

# --- 1. Compilar binario ---
Write-Host "`n[1/5] Compilando lfsoft-sync.exe..." -ForegroundColor Yellow
Push-Location $root
try {
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    go build -ldflags "-s -w" -o "$deploy\lfsoft-sync.exe" ./cmd/sync
    if ($LASTEXITCODE -ne 0) { throw "go build fallo" }
    Write-Host "  OK: lfsoft-sync.exe" -ForegroundColor Green
} finally {
    Pop-Location
    Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
}

# --- 2. Generar sync.yaml para el servidor ---
Write-Host "`n[2/5] Generando config\sync.yaml..." -ForegroundColor Yellow
$yamlContent = @"
sync_port: $SyncPort
pg_dsn: "$PgDSN"
mdb_base_path: "$($MdbBasePath -replace '\\', '\\')"
entities_dir: "C:\\Apps\\LFSoft\\tools\\sync\\config\\entities"
"@
Set-Content -Path "$deploy\config\sync.yaml" -Value $yamlContent -Encoding utf8
Write-Host "  OK: sync.yaml (mdb_base_path: $MdbBasePath)" -ForegroundColor Green

# --- 3. Copiar entity YAMLs ---
Write-Host "`n[3/5] Copiando entity YAMLs..." -ForegroundColor Yellow
$entities = Get-ChildItem "$root\config\entities\*.yaml"
foreach ($f in $entities) {
    Copy-Item $f.FullName "$deploy\config\entities\"
    Write-Host "  + $($f.Name)"
}

# --- 4. Copiar migraciones ---
Write-Host "`n[4/5] Copiando migraciones SQL..." -ForegroundColor Yellow
$syncMigrations = Get-ChildItem "$root\migrations\*.sql"
foreach ($f in $syncMigrations) {
    Copy-Item $f.FullName "$deploy\migrations\"
    Write-Host "  + $($f.Name)"
}

$appMigrationPath = "C:\Desarrollo\LFSoft-app\database\articulos\migrations\V025__alter_identity_by_default.up.sql"
if (Test-Path $appMigrationPath) {
    Copy-Item $appMigrationPath "$deploy\migrations-app\"
    Write-Host "  + V025__alter_identity_by_default.up.sql (app migration)"
} else {
    Write-Host "  WARN: V025 no encontrada en $appMigrationPath" -ForegroundColor DarkYellow
}

# --- 5. Copiar script de instalacion y NSSM ---
Write-Host "`n[5/5] Copiando scripts de instalacion..." -ForegroundColor Yellow
Copy-Item "$root\scripts\install-server.ps1" "$deploy\"
Write-Host "  + install-server.ps1"

# --- Resumen ---
$size = (Get-ChildItem $deploy -Recurse | Measure-Object -Property Length -Sum).Sum / 1MB
Write-Host "`n=== Deploy package listo ===" -ForegroundColor Cyan
Write-Host "Carpeta: $deploy"
Write-Host "Tamano:  $([math]::Round($size, 2)) MB"
Write-Host "`nArchivos:"
Get-ChildItem $deploy -Recurse -File | ForEach-Object {
    $rel = $_.FullName.Substring($deploy.Length + 1)
    Write-Host "  $rel"
}

Write-Host "`n--- Siguiente paso ---" -ForegroundColor Yellow
Write-Host "1. Copiar la carpeta '$OutputDir' al servidor (ej: \\servidor\c$\temp\deploy)"
Write-Host "2. En el servidor, abrir PowerShell como Administrador"
Write-Host "3. Ejecutar: .\install-server.ps1"
Write-Host ""
