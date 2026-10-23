<#
.SYNOPSIS
    Mantenimiento nocturno del sync (tarea programada "LFSoft Sync - Mantenimiento").

.DESCRIPTION
    1. Detiene el servicio del sync.
    2. Rota logs\sync.log (sync_<fecha>.log) y borra los rotados de mas de -DiasLogs dias.
    3. Purga sync.sync_log (mas de -DiasHistorial dias) y los cambios ya procesados de
       sync.pg_changes (mas de 7 dias): el sync agrega filas en cada ciclo y nadie las borra.
    4. Vuelve a arrancar el servicio. El primer barrido de cada entidad despues de arrancar
       compara todas las filas de Access con PG: es la reconciliacion completa diaria.

    Lo que hace queda en logs\mantenimiento.log.
#>
param(
    [string]$SyncInstallDir = "C:\Apps\LFSoft\tools\sync",
    [string]$ServiceName = "LFSoftSync",
    [string]$PgBin = "C:\Apps\LFSoft\tools\postgresql\bin",
    [int]$DiasLogs = 30,
    [int]$DiasHistorial = 30
)

$ErrorActionPreference = "Continue"
$logDir = Join-Path $SyncInstallDir "logs"
$maintLog = Join-Path $logDir "mantenimiento.log"
function Log([string]$msg) { Add-Content -Path $maintLog -Value ("{0}  {1}" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"), $msg) }

Log "inicio"
$svc = Get-Service $ServiceName -ErrorAction SilentlyContinue
if (-not $svc) { Log "el servicio $ServiceName no existe; nada que hacer"; exit 1 }

if ($svc.Status -ne 'Stopped') {
    Stop-Service $ServiceName -Force
    $svc.WaitForStatus('Stopped', (New-TimeSpan -Seconds 60))
}
Log "servicio detenido"

# Rotacion: con el servicio detenido el archivo no esta abierto.
$current = Join-Path $logDir "sync.log"
if (Test-Path $current) {
    $rotated = Join-Path $logDir ("sync_{0}.log" -f (Get-Date -Format "yyyyMMdd_HHmm"))
    Move-Item $current $rotated -Force
    Log "log rotado a $rotated"
}
Get-ChildItem (Join-Path $logDir "sync_*.log") -ErrorAction SilentlyContinue |
    Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-$DiasLogs) } |
    ForEach-Object { Remove-Item $_.FullName -Force; Log "borrado $($_.Name)" }

# Purga de las tablas de control del sync.
$yaml = Join-Path $SyncInstallDir "config\sync.yaml"
$psql = Join-Path $PgBin "psql.exe"
if ((Test-Path $yaml) -and (Test-Path $psql)) {
    $text = [System.IO.File]::ReadAllText($yaml)
    if ($text -match '(?m)^\s*pg_dsn\s*:\s*["'']?([^"''\r\n]+)') {
        $dsn = $Matches[1].Trim()
        $sql = "DELETE FROM sync.sync_log WHERE sl_startedAt < NOW() - INTERVAL '$DiasHistorial days';" +
               "DELETE FROM sync.pg_changes WHERE ch_processed AND ch_at < NOW() - INTERVAL '7 days';"
        $tmp = [System.IO.Path]::GetTempFileName()
        [System.IO.File]::WriteAllText($tmp, $sql)
        $out = & $psql -X -q -v ON_ERROR_STOP=1 -d $dsn -f $tmp 2>&1 | ForEach-Object { "$_" }
        Log ("purga de sync.sync_log y sync.pg_changes: exit {0} {1}" -f $LASTEXITCODE, ($out -join ' '))
        Remove-Item $tmp -ErrorAction SilentlyContinue
    } else { Log "sin pg_dsn en sync.yaml: no se purgaron las tablas de control" }
} else { Log "falta sync.yaml o psql.exe ($psql): no se purgaron las tablas de control" }

Start-Service $ServiceName
Start-Sleep -Seconds 5
Log ("servicio: {0}" -f (Get-Service $ServiceName).Status)
Log "fin"
