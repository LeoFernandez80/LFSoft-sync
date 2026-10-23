<#
.SYNOPSIS
    Arma el paquete de reset (maquina de desarrollo): C:\Desarrollo\LFSoft-reset_<fecha>[.zip].

.DESCRIPTION
    Junta en una carpeta lista para copiar al servidor:
      - Reset-LFSoft.ps1, Restore-LFSoft.ps1, Mantenimiento-Sync.ps1, Aplicar-Grillas.ps1, LEEME.md,
        sql\ (con sql\grillas\, un script por grilla)
      - manifest-app.txt (regenerado desde LFSoft-app\database) y migrations-app\<dominio>\*.up.sql
      - sync\lfsoft-sync.exe (compilado ahora, windows/amd64), sync\config (de deploy\config) y
        sync\migrations (de deploy\migrations)

    Arma ademas, aparte, el paquete de grillas C:\Desarrollo\LFSoft-grillas_<fecha>[.zip]: solo
    Aplicar-Grillas.ps1, sql\grillas\ y su LEEME (LEEME-Grillas.md), para aplicar en el servidor la
    configuracion y las columnas visibles de las grillas sin el resto del reset.

    No toca deploy\ ni scripts\deploy-build.ps1.

.PARAMETER SoloGrillas
    Arma solo el paquete de grillas (no compila el sync ni junta las migraciones).

.EXAMPLE
    .\deploy\reset\Build-ResetPackage.ps1
    .\deploy\reset\Build-ResetPackage.ps1 -AppRepo C:\Desarrollo\LFSoft-app -Zip
    .\deploy\reset\Build-ResetPackage.ps1 -SoloGrillas -Zip
#>
param(
    [string]$AppRepo = "C:\Desarrollo\LFSoft-app",
    [string]$OutRoot = "C:\Desarrollo",
    [string]$Name = "",
    [switch]$Zip,
    [switch]$SoloGrillas
)

$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$repo = (Resolve-Path (Join-Path $here "..\..")).Path

function Write-Zip([string]$dir) {
    $zipPath = "$dir.zip"
    if (Test-Path $zipPath) { Remove-Item $zipPath -Force }
    Compress-Archive -Path (Join-Path $dir "*") -DestinationPath $zipPath
    Write-Host "Zip: $zipPath"
}

# --- Paquete de grillas: Aplicar-Grillas.ps1 + sql\grillas, sin el resto del reset ---
$grOut = Join-Path $OutRoot ("LFSoft-grillas_" + (Get-Date -Format "yyyyMMdd"))
if (Test-Path $grOut) { Remove-Item $grOut -Recurse -Force }
New-Item -ItemType Directory -Force (Join-Path $grOut "sql") | Out-Null
Copy-Item (Join-Path $here "Aplicar-Grillas.ps1") $grOut
Copy-Item (Join-Path $here "Ocultar-CamposId.ps1") $grOut
Copy-Item (Join-Path $here "LEEME-Grillas.md") (Join-Path $grOut "LEEME.md")
Copy-Item (Join-Path $here "sql\grillas") (Join-Path $grOut "sql") -Recurse
Copy-Item (Join-Path $here "sql\campos-id-ocultos.sql") (Join-Path $grOut "sql")
Write-Host "Grillas: $((Get-ChildItem (Join-Path $grOut 'sql\grillas\*.sql')).Count)"
if ($Zip) { Write-Zip $grOut }
Write-Host "Paquete de grillas: $grOut" -ForegroundColor Green
if ($SoloGrillas) { return }

if (-not $Name) { $Name = "LFSoft-reset_" + (Get-Date -Format "yyyyMMdd") }
$out = Join-Path $OutRoot $Name
$db = Join-Path $AppRepo "database"
if (-not (Test-Path $db)) { throw "No existe $db" }

if (Test-Path $out) { Remove-Item $out -Recurse -Force }
New-Item -ItemType Directory -Force $out | Out-Null

# --- Manifiesto: mismo orden que el que se probo; se regenera para tomar migraciones nuevas ---
# Rangos fijos por dependencias cruzadas (ver cabecera del manifiesto).
function Get-Ups([string]$domain, [int]$from, [int]$to) {
    Get-ChildItem (Join-Path $db "$domain\migrations\*.up.sql") | Sort-Object Name | Where-Object {
        $v = [int]$_.Name.Substring(1, 3); $v -ge $from -and $v -le $to
    }
}
$lines = @(
    "# Orden de aplicacion de las migraciones de LFSoft-app sobre una base vacia.",
    "# Formato: <schema del search_path>|<dominio (carpeta)>|<archivo>. Schema `"-`" = no se ejecuta.",
    "# El orden intercala dominios por sus referencias cruzadas:",
    "#   seguridad V027 -> settings.literal_keys; utilities V002 -> settings.module_options;",
    "#   settings V098 -> utilities.banks; sales V009 -> seguridad.users; sales V010 -> articulos.actividades;",
    "#   sales V014 -> catalogos de utilities; articulos V017 -> sales.customers; sales V025 -> articulos.stockMovements.",
    "# access_control vive en plataforma (unificada en la Fase 1): articulos V012 se aplica ahi y su gemela",
    "# seguridad V023 (copia de la epoca database-per-service) no se ejecuta.",
    "# No se ejecutan las migraciones de DATOS del negocio: los datos los carga el sync desde Access.",
    "#   articulos V014 (actividades) y V015 (paridades), utilities V020 (catalogos de clientes),",
    "#   sales V021 (clientes) y V023 (remapeo de datos de presupuestos).",
    "# Si se ejecutan las de configuracion de la app (seguridad y settings: usuario admin, permisos,",
    "# literales, grillas) y los dos catalogos fijos del codigo VB6 sin tabla en Access",
    "# (precios V006 monedas, articulos V024 tipos de movimiento de stock)."
)
$dataSeeds = @('articulos|14', 'articulos|15', 'utilities|20', 'sales|21', 'sales|23')
$plan = @(
    @('settings', 1, 97), @('seguridad', 1, 999), @('articulos', 1, 16), @('precios', 1, 999),
    @('utilities', 1, 999), @('settings', 98, 999), @('sales', 1, 24), @('articulos', 17, 999), @('sales', 25, 999)
)
$count = 0
foreach ($p in $plan) {
    foreach ($f in (Get-Ups $p[0] $p[1] $p[2])) {
        $v = [int]$f.Name.Substring(1, 3)
        $schema = $p[0]
        if ($p[0] -eq 'seguridad' -and $v -eq 23) { $schema = '-' }
        if ($p[0] -eq 'articulos' -and $v -eq 12) { $schema = 'plataforma' }
        if ($dataSeeds -contains "$($p[0])|$v") { $schema = '-' }
        $lines += "$schema|$($p[0])|$($f.Name)"
        $dest = Join-Path $out "migrations-app\$($p[0])"
        New-Item -ItemType Directory -Force $dest | Out-Null
        if ($schema -ne "-") { Copy-Item $f.FullName $dest }
        $count++
    }
}
[System.IO.File]::WriteAllText((Join-Path $out "manifest-app.txt"), ($lines -join "`r`n") + "`r`n", (New-Object System.Text.UTF8Encoding($false)))
Copy-Item (Join-Path $out "manifest-app.txt") (Join-Path $here "manifest-app.txt") -Force
Write-Host "Migraciones de la app: $count"

# --- Scripts ---
foreach ($f in @("Reset-LFSoft.ps1", "Restore-LFSoft.ps1", "Mantenimiento-Sync.ps1", "Aplicar-Grillas.ps1", "Ocultar-CamposId.ps1", "LEEME.md")) {
    Copy-Item (Join-Path $here $f) $out
}
Copy-Item (Join-Path $here "sql") $out -Recurse

# --- Sync ---
$syncOut = Join-Path $out "sync"
New-Item -ItemType Directory -Force (Join-Path $syncOut "config\entities"), (Join-Path $syncOut "migrations") | Out-Null
Push-Location $repo
try {
    $env:GOOS = "windows"; $env:GOARCH = "amd64"
    & go build -o (Join-Path $syncOut "lfsoft-sync.exe") ./cmd/sync
    if ($LASTEXITCODE -ne 0) { throw "go build fallo" }
} finally {
    Remove-Item Env:\GOOS, Env:\GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}
Copy-Item (Join-Path $repo "deploy\config\sync.yaml") (Join-Path $syncOut "config")
Copy-Item (Join-Path $repo "deploy\config\entities\*.yaml") (Join-Path $syncOut "config\entities")
Copy-Item (Join-Path $repo "deploy\migrations\*.sql") (Join-Path $syncOut "migrations")
$hash = (Get-FileHash (Join-Path $syncOut "lfsoft-sync.exe") -Algorithm SHA256).Hash.ToLower()
Write-Host "lfsoft-sync.exe SHA256: $hash"
$leeme = Join-Path $out "LEEME.md"
[System.IO.File]::WriteAllText($leeme, [System.IO.File]::ReadAllText($leeme).Replace("{{SHA256}}", $hash), (New-Object System.Text.UTF8Encoding($false)))
Write-Host "Entidades: $((Get-ChildItem (Join-Path $syncOut 'config\entities\*.yaml')).Count)"

if ($Zip) { Write-Zip $out }
Write-Host "Paquete: $out" -ForegroundColor Green
