<#
.SYNOPSIS
    Deja visibles solo las columnas mas relevantes de las grillas de la app (sql\grillas\*.sql).

.DESCRIPTION
    Cada grilla tiene su script en sql\grillas\<grilla>.sql (por ejemplo eGrid_Customers.sql: razon
    social, CUIT y estado). Cada script cambia el visible del catalogo de columnas (lo que ve quien
    nunca configuro la grilla) y el de las configuraciones que los usuarios ya guardaron para esa
    grilla. Las columnas ocultas se pueden volver a mostrar desde la configuracion de la grilla.
    Ademas le da al rol ADMIN el engranaje de configuracion de la grilla (eAction_ConfigureGrid).

    Reset-LFSoft.ps1 los aplica todos en el paso 7. Este script sirve para aplicarlos (todos o
    algunos) sobre una base ya en uso. No hace falta detener el backend: la grilla toma el cambio
    la proxima vez que se abre el modulo. Se puede correr mas de una vez.

.PARAMETER Grilla
    Grillas a aplicar, con o sin el prefijo eGrid_ (Customers, eGrid_Quotes). Sin este parametro
    se aplican todas.
.PARAMETER Listar
    Muestra las grillas disponibles y las columnas que deja visibles. No toca la base.
.PARAMETER DatabaseUrl
    Conexion a la base. Por defecto, el pg_dsn del sync instalado.

.EXAMPLE
    .\Aplicar-Grillas.ps1 -Listar
    .\Aplicar-Grillas.ps1
    .\Aplicar-Grillas.ps1 -Grilla Customers, Quotes
#>
[CmdletBinding(PositionalBinding = $false)]
param(
    [string[]]$Grilla = @(),
    [switch]$Listar,
    [string]$DatabaseUrl = "",
    [string]$PgBin = "",
    [string]$SyncInstallDir = "C:\Apps\LFSoft\tools\sync"
)

$ErrorActionPreference = "Stop"
$dir = Join-Path $PSScriptRoot "sql\grillas"
function Write-Fail([string]$msg) { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }
function Write-Ok([string]$msg) { Write-Host "  $msg" -ForegroundColor Green }
function Write-Warn([string]$msg) { Write-Host "  ADVERTENCIA: $msg" -ForegroundColor DarkYellow }

$all = @(Get-ChildItem (Join-Path $dir "*.sql") -ErrorAction SilentlyContinue | Sort-Object Name)
if ($all.Count -eq 0) { Write-Fail "No hay scripts en $dir." }

if ($Listar) {
    foreach ($f in $all) {
        $first = [System.IO.File]::ReadAllLines($f.FullName)[0]
        Write-Host ($first -replace '^-- Grilla ', '')
    }
    exit 0
}

$files = $all
if ($Grilla.Count -gt 0) {
    $files = @()
    foreach ($g in $Grilla) {
        $name = if ($g -like 'eGrid_*') { $g } else { "eGrid_$g" }
        $match = $all | Where-Object { $_.BaseName -eq $name }
        if (-not $match) { Write-Fail "No hay script para la grilla '$g'. Ver .\Aplicar-Grillas.ps1 -Listar" }
        $files += $match
    }
}

# --- psql ------------------------------------------------------------------------------
if (-not $PgBin) {
    $candidates = @("C:\Apps\LFSoft\tools\postgresql\bin")
    $candidates += @(Get-ChildItem "C:\Program Files\PostgreSQL\*\bin" -Directory -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | ForEach-Object { $_.FullName })
    $onPath = Get-Command psql -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($onPath -and $onPath.Source) { $candidates += (Split-Path $onPath.Source) }
    $PgBin = $candidates | Where-Object { $_ -and (Test-Path (Join-Path $_ "psql.exe")) } | Select-Object -First 1
}
if (-not $PgBin) { Write-Fail "No se encontro psql.exe. Pasar la carpeta bin con -PgBin." }
$psql = Join-Path $PgBin "psql.exe"

# --- Conexion: la del sync instalado ------------------------------------------------------
if (-not $DatabaseUrl) {
    $yaml = Join-Path $SyncInstallDir "config\sync.yaml"
    if (-not (Test-Path $yaml)) { Write-Fail "No esta $yaml. Pasar -DatabaseUrl." }
    if ([System.IO.File]::ReadAllText($yaml) -match "(?m)^\s*pg_dsn\s*:\s*[`"']?([^`"'\r\n]+)[`"']?") { $DatabaseUrl = $Matches[1].Trim() }
    if (-not $DatabaseUrl) { Write-Fail "No hay pg_dsn en $yaml. Pasar -DatabaseUrl." }
}
# psql no entiende los parametros propios de pgx (search_path, pool_*).
$keep = @('sslmode', 'sslrootcert', 'sslcert', 'sslkey', 'connect_timeout', 'application_name', 'host', 'port', 'user', 'password', 'dbname')
$parts = $DatabaseUrl.Split('?', 2)
$conn = $parts[0]
if ($parts.Count -eq 2) {
    $kept = @($parts[1].Split('&') | Where-Object { $keep -contains ($_.Split('=', 2)[0]) })
    if ($kept.Count -gt 0) { $conn += '?' + ($kept -join '&') }
}
$env:PGCLIENTENCODING = "UTF8"
$env:PGOPTIONS = "-c client_min_messages=warning"

# --- Aplicar ------------------------------------------------------------------------------
$failed = 0
foreach ($f in $files) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    $out = & $psql -X -q -v ON_ERROR_STOP=1 -d $conn -f $f.FullName 2>&1 | ForEach-Object { "$_" }
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    if ($code -ne 0) {
        $failed++
        Write-Warn "$($f.BaseName): $($out -join ' ')"
    } else {
        Write-Ok $f.BaseName
        foreach ($l in $out) { if ($l) { Write-Warn $l } }
    }
}
if ($failed -gt 0) { Write-Fail "$failed de $($files.Count) grilla(s) no se pudieron aplicar." }
Write-Host "Grillas aplicadas: $($files.Count)" -ForegroundColor Green
