<#
.SYNOPSIS
    Oculta los campos ID de las entidades en los filtros y forms de la app, para todos los roles.

.DESCRIPTION
    Aplica sql\campos-id-ocultos.sql: agrega el <entidad>_id a los campos ocultos
    (seguridad.role_hidden_fields) de cada rol en cada filtro (eFilter_*) y form (eForm_*) que lo
    muestra, sin quitar los campos que el rol ya tenga ocultos. Es solo presentacion: el ID sigue
    en el JSON y la app lo sigue usando. Se puede volver a mostrar con -Revertir, o por rol y
    pantalla desde el ABM de campos ocultos.

    No hace falta detener el backend: cada usuario ve el cambio al recargar la app (F5) o al
    volver a entrar. Se puede correr mas de una vez. Reset-LFSoft.ps1 lo aplica en el paso 7.

.PARAMETER Revertir
    Quita el campo ID de los campos ocultos (vuelve a mostrarse).
.PARAMETER DatabaseUrl
    Conexion a la base. Por defecto, el pg_dsn del sync instalado.

.EXAMPLE
    .\Ocultar-CamposId.ps1
    .\Ocultar-CamposId.ps1 -Revertir
    .\Ocultar-CamposId.ps1 -DatabaseUrl "postgres://usuario:clave@localhost:5432/lfsoft_hidramic"
#>
[CmdletBinding(PositionalBinding = $false)]
param(
    [switch]$Revertir,
    [string]$DatabaseUrl = "",
    [string]$PgBin = "",
    [string]$SyncInstallDir = "C:\Apps\LFSoft\tools\sync"
)

$ErrorActionPreference = "Stop"
$sql = Join-Path $PSScriptRoot "sql\campos-id-ocultos.sql"
function Write-Fail([string]$msg) { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }
function Write-Warn([string]$msg) { Write-Host "  ADVERTENCIA: $msg" -ForegroundColor DarkYellow }

if (-not (Test-Path $sql)) { Write-Fail "No esta $sql." }

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
$env:PGOPTIONS = "-c client_min_messages=notice -c lfsoft.revertir=" + $(if ($Revertir) { "on" } else { "off" })

# --- Aplicar ------------------------------------------------------------------------------
$prev = $ErrorActionPreference
$ErrorActionPreference = "Continue"
$out = & $psql -X -q -v ON_ERROR_STOP=1 -d $conn -f $sql 2>&1 | ForEach-Object { "$_" }
$code = $LASTEXITCODE
$ErrorActionPreference = $prev
if ($code -ne 0) { Write-Fail ($out -join "`n") }
foreach ($l in $out) {
    if ($l -match 'WARNING:\s*(.*)') { Write-Warn $Matches[1] }
    elseif ($l -match 'NOTICE:\s*(.*)') { Write-Host "  $($Matches[1])" }
}
if ($Revertir) { Write-Host "Campos ID visibles otra vez en filtros y forms." -ForegroundColor Green }
else { Write-Host "Campos ID ocultos en filtros y forms. Los usuarios lo ven al recargar la app (F5)." -ForegroundColor Green }
