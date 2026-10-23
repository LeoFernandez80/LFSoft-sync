<#
.SYNOPSIS
    Vuelve la base (y la instalacion del sync) al estado del backup que hizo Reset-LFSoft.ps1.

.DESCRIPTION
    Ejecutar como Administrador. Detiene el backend y el sync, borra todos los schemas de la base,
    restaura el pg_dump del backup y la instalacion anterior del sync, y vuelve a arrancar los
    servicios.

    Los .mdb NO se restauran salvo -RestaurarMdb: el legacy sigue trabajando sobre ellos durante y
    despues del reset, y pisarlos con la copia perderia lo cargado desde entonces.

.EXAMPLE
    .\Restore-LFSoft.ps1 -Backup C:\Apps\LFSoft\backups\reset_20261016_220000
#>
[CmdletBinding(PositionalBinding = $false)]
param(
    [Parameter(Mandatory = $true)][string]$Backup,
    [string]$DatabaseUrl = "",
    [string]$DatabaseName = "lfsoft_hidramic",
    [string]$PgBin = "",
    [string]$SyncInstallDir = "C:\Apps\LFSoft\tools\sync",
    [string]$SyncServiceName = "LFSoftSync",
    [string]$BackendServiceName = "",
    [string]$MdbPath = "",
    [switch]$RestaurarMdb,
    [switch]$Forzar
)

$ErrorActionPreference = "Stop"
$utf8 = New-Object System.Text.UTF8Encoding($false)
function Write-Fail([string]$msg) { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }
function Write-Ok([string]$msg) { Write-Host "  $msg" -ForegroundColor Green }

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { Write-Fail "Ejecutar PowerShell como Administrador." }

$dump = Join-Path $Backup "$DatabaseName.dump"
if (-not (Test-Path $dump)) { Write-Fail "No esta $dump." }

if (-not $PgBin) {
    $candidates = @("C:\Apps\LFSoft\tools\postgresql\bin") + @(Get-ChildItem "C:\Program Files\PostgreSQL\*\bin" -Directory -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | ForEach-Object { $_.FullName })
    $PgBin = $candidates | Where-Object { Test-Path (Join-Path $_ "pg_restore.exe") } | Select-Object -First 1
}
if (-not $PgBin) { Write-Fail "No se encontro pg_restore.exe. Pasar -PgBin." }
$psql = Join-Path $PgBin "psql.exe"
$pgRestore = Join-Path $PgBin "pg_restore.exe"

# La conexion sale del sync.yaml respaldado (el que estaba instalado antes del reset).
$yaml = @((Join-Path $Backup "sync-instalado\config\sync.yaml"), (Join-Path $SyncInstallDir "config\sync.yaml")) | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $DatabaseUrl -and $yaml) {
    $t = [System.IO.File]::ReadAllText($yaml)
    if ($t -match '(?m)^\s*pg_dsn\s*:\s*["'']?([^"''\r\n]+)') { $DatabaseUrl = $Matches[1].Trim() }
    if (-not $MdbPath -and $t -match '(?m)^\s*mdb_base_path\s*:\s*["'']?([^"''\r\n]+)') { $MdbPath = $Matches[1].Trim() -replace '\\\\', '\' }
}
if (-not $DatabaseUrl) { Write-Fail "Pasar -DatabaseUrl." }
$env:PGCLIENTENCODING = "UTF8"
$env:PGOPTIONS = "-c client_min_messages=warning"

function Invoke-Native([string]$exe, [string[]]$arguments) {
    $prev = $ErrorActionPreference; $ErrorActionPreference = "Continue"
    $out = & $exe @arguments 2>&1 | ForEach-Object { "$_" }
    $code = $LASTEXITCODE; $ErrorActionPreference = $prev
    return [pscustomobject]@{ Code = $code; Output = ($out -join "`n") }
}
function Invoke-Sql([string]$sql) {
    $tmp = [System.IO.Path]::GetTempFileName()
    [System.IO.File]::WriteAllText($tmp, $sql, $utf8)
    $r = Invoke-Native $psql @('-X', '-qtA', '-v', 'ON_ERROR_STOP=1', '-d', $DatabaseUrl, '-f', $tmp)
    Remove-Item $tmp -ErrorAction SilentlyContinue
    if ($r.Code -ne 0) { Write-Fail "Fallo la consulta:`n$($r.Output)" }
    return $r.Output.Trim()
}

$db = Invoke-Sql "SELECT current_database() || '|' || current_setting('server_version_num')"
$dbName = $db.Split('|')[0]; $pgVersion = [int]$db.Split('|')[1]
if ($dbName -ne $DatabaseName) { Write-Fail "La conexion apunta a '$dbName', no a '$DatabaseName'." }
Write-Host "Restaurar $dbName desde $dump" -ForegroundColor Cyan
if (-not $Forzar) {
    $a = Read-Host "Se borra el contenido actual de la base. Escribir el nombre de la base para confirmar"
    if ($a -ne $dbName) { Write-Fail "Confirmacion incorrecta. No se modifico nada." }
}

# Servicios: backend (NSSM con DATABASE_URL a esta base) y sync.
$backends = @()
Get-ChildItem "HKLM:\SYSTEM\CurrentControlSet\Services" -ErrorAction SilentlyContinue | ForEach-Object {
    $p = Get-ItemProperty (Join-Path $_.PSPath "Parameters") -ErrorAction SilentlyContinue
    if ($p -and $p.Application -and $p.AppEnvironmentExtra -and ($p.Application -notmatch 'sync')) {
        $dbLine = @($p.AppEnvironmentExtra) | Where-Object { $_ -like "DATABASE_URL=*" } | Select-Object -First 1
        if ($dbLine -and ($dbLine -match "/$([regex]::Escape($DatabaseName))(\?|$)")) { $backends += $_.PSChildName }
    }
}
if ($BackendServiceName) { $backends = @($BackendServiceName) }
foreach ($s in $backends + @($SyncServiceName)) {
    $svc = Get-Service $s -ErrorAction SilentlyContinue
    if ($svc -and $svc.Status -ne 'Stopped') { Stop-Service $s -Force; Write-Ok "Detenido $s" }
}

$public = if ($pgVersion -ge 150000) { "CREATE SCHEMA public AUTHORIZATION pg_database_owner; GRANT USAGE ON SCHEMA public TO PUBLIC;" } else { "CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO PUBLIC;" }
$null = Invoke-Sql @"
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid();
DO `$`$
DECLARE s RECORD;
BEGIN
    FOR s IN SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
    LOOP
        EXECUTE format('DROP SCHEMA %I CASCADE', s.nspname);
    END LOOP;
END `$`$;
$public
SELECT 1;
"@
Write-Ok "Base vaciada"

# public ya existe: pg_restore avisa "schema public already exists" y sigue; no es un error real.
$r = Invoke-Native $pgRestore @('-d', $DatabaseUrl, '--no-owner', '-j', '2', $dump)
$errs = @(($r.Output -split "`n") | Where-Object { $_ -match 'error' -and $_ -notmatch 'schema "public" already exists' -and $_ -notmatch 'errors ignored on restore' })
if ($errs.Count -gt 0) { Write-Host $r.Output; Write-Host "  pg_restore informo errores (arriba). Revisarlos." -ForegroundColor DarkYellow }
else { Write-Ok "Base restaurada" }

$syncBackup = Join-Path $Backup "sync-instalado"
if (Test-Path $syncBackup) {
    Get-ChildItem (Join-Path $SyncInstallDir "config\entities\*.yaml") -ErrorAction SilentlyContinue | Remove-Item -Force
    Get-ChildItem (Join-Path $SyncInstallDir "migrations\*.sql") -ErrorAction SilentlyContinue | Remove-Item -Force
    Copy-Item (Join-Path $syncBackup "*") $SyncInstallDir -Recurse -Force
    Write-Ok "Instalacion anterior del sync restaurada en $SyncInstallDir"
}

if ($RestaurarMdb) {
    if (-not $MdbPath) { Write-Fail "Pasar -MdbPath para restaurar los .mdb." }
    Get-ChildItem (Join-Path $Backup "mdb\*.mdb") | ForEach-Object { Copy-Item $_.FullName $MdbPath -Force; Write-Ok "Restaurado $($_.Name)" }
}

foreach ($s in @($SyncServiceName) + $backends) {
    if (Get-Service $s -ErrorAction SilentlyContinue) { Start-Service $s; Write-Ok "Arrancado $s" }
}
Write-Host "Restauracion terminada." -ForegroundColor Cyan
