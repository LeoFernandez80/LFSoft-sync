<#
.SYNOPSIS
    Recrea desde cero la base de LFSoft-app (lfsoft_hidramic) y la deja sincronizada con el legacy.

.DESCRIPTION
    Ejecutar como Administrador en el servidor, parado en la carpeta del paquete. Pasos:

      1. Verificaciones previas (psql/pg_dump, conexion, nombre de la base, carpeta de los .mdb).
      2. Confirmacion: hay que escribir el nombre de la base (salvo -Forzar).
      3. Detiene el backend (servicio NSSM) y el servicio del sync.
      4. Backup: pg_dump completo de la base (formato custom, verificado con pg_restore -l),
         copia de los .mdb que usa el sync y de la instalacion actual del sync.
      5. Borra TODOS los schemas de la base (incluido public) y los vuelve a crear vacios.
      6. Aplica las migraciones de estructura de la app en el orden de manifest-app.txt (las de
         datos del negocio no: esos datos los carga el sync) y las registra en
         public.lfsoft_schema_migrations (las que use el proximo Apply-Migrations.ps1).
      7. Restaura los usuarios ADMIN con sus permisos y la tabla settings.branding del backup, y deja
         visibles solo las columnas mas relevantes de cada grilla (sql\grillas, salvo -SinGrillas).
      8. Instala el sync (exe, config, entidades, migraciones) y corre sus migraciones.
      9. Carga inicial: una pasada completa Access -> PG de todas las entidades, en orden.
     10. Instala y arranca el servicio del sync (barridos periodicos = sincronizaciones croneadas)
         y la tarea programada de mantenimiento nocturno; vuelve a arrancar el backend.

    Todo queda en logs\reset_<fecha>.log dentro de la carpeta del paquete.
    Para volver atras: .\Restore-LFSoft.ps1 -Backup <carpeta del backup>.

.PARAMETER DatabaseUrl
    Conexion a la base. Por defecto, el pg_dsn del sync instalado (o el del paquete).
.PARAMETER DatabaseName
    Nombre esperado de la base: si la conexion apunta a otra, el script se detiene.
.PARAMETER SoloVerificar
    Corre solo las verificaciones previas y muestra lo que haria. No toca nada.
.PARAMETER Forzar
    No pide la confirmacion interactiva.
.PARAMETER SinBackupMdb
    No copia los .mdb al backup (el backup de PG se hace igual, siempre).
.PARAMETER DesdeCargaInicial
    Retoma un reset que se corto despues de las migraciones: no hace backup, no borra ni migra;
    reinstala el sync, corre la carga inicial y arranca los servicios (incluido el backend).
.PARAMETER SinGrillas
    No aplica sql\grillas: las grillas quedan con todas sus columnas visibles (se pueden aplicar
    despues con .\Aplicar-Grillas.ps1).
.PARAMETER SinServicios
    No detiene ni crea servicios ni la tarea programada (el backend y el sync tienen que estar
    detenidos de antes). Hace base, migraciones, instalacion de archivos y carga inicial.

.EXAMPLE
    .\Reset-LFSoft.ps1 -SoloVerificar
    .\Reset-LFSoft.ps1
    .\Reset-LFSoft.ps1 -DatabaseUrl "postgres://postgres:clave@localhost:5432/lfsoft_hidramic"
#>
[CmdletBinding(PositionalBinding = $false)]
param(
    [string]$DatabaseUrl = "",
    [string]$DatabaseName = "lfsoft_hidramic",
    [string]$PgBin = "",
    [string]$MdbPath = "",
    [string]$BackupRoot = "C:\Apps\LFSoft\backups",
    [string]$SyncInstallDir = "C:\Apps\LFSoft\tools\sync",
    [string]$SyncServiceName = "LFSoftSync",
    [string]$BackendServiceName = "",
    [string]$MaintenanceTime = "03:30",
    [switch]$SoloVerificar,
    [switch]$Forzar,
    [switch]$SinBackupMdb,
    [switch]$SinServicios,
    [switch]$SinGrillas,
    [switch]$DesdeCargaInicial
)

$ErrorActionPreference = "Stop"
$pkg = $PSScriptRoot
$stamp = Get-Date -Format "yyyyMMdd_HHmmss"
$utf8 = New-Object System.Text.UTF8Encoding($false)
$appSchemas = @('articulos', 'precios', 'sales', 'seguridad', 'settings', 'utilities', 'plataforma')
$tracking = 'public."lfsoft_schema_migrations"'

New-Item -ItemType Directory -Force (Join-Path $pkg "logs") | Out-Null
$logFile = Join-Path $pkg "logs\reset_$stamp.log"
try { Start-Transcript -Path $logFile -Append | Out-Null } catch { }

# La consola clasica de Windows no siempre puede escribir los acentos que devuelve psql.
$script:origOutEnc = $null
# No se pasa la consola a UTF-8: la consola clasica de Windows falla (error Win32 0x1F) al escribir
# algunos caracteres y corta el script. Lo que viene de procesos externos se escribe con Write-Safe.

# Write-Safe escribe una linea sin cortar el script si la consola no puede mostrarla: reintenta
# sin los caracteres que no son ASCII.
function Write-Safe([string]$text) {
    try { Write-Host $text }
    catch {
        $ascii = -join ($text.Normalize([Text.NormalizationForm]::FormD).ToCharArray() | ForEach-Object { if ([int]$_ -lt 128) { $_ } elseif ([Globalization.CharUnicodeInfo]::GetUnicodeCategory($_) -ne 'NonSpacingMark') { '?' } })
        try { Write-Host $ascii } catch { }
    }
}

function Restore-Console { if ($script:origOutEnc) { try { [Console]::OutputEncoding = $script:origOutEnc } catch { } } }
function Write-Step([string]$msg) { Write-Host ""; Write-Host "=== $msg ===" -ForegroundColor Cyan }
function Write-Ok([string]$msg) { Write-Host "  $msg" -ForegroundColor Green }
function Write-Warn([string]$msg) { Write-Host "  ADVERTENCIA: $msg" -ForegroundColor DarkYellow }
function Write-Fail([string]$msg) {
    Write-Safe ""; Write-Safe "ERROR: $msg"
    if ($script:backupDir) {
        Write-Host "El backup de este intento esta en: $script:backupDir" -ForegroundColor Yellow
        Write-Host "Para volver al estado anterior: .\Restore-LFSoft.ps1 -Backup `"$script:backupDir`"" -ForegroundColor Yellow
    }
    Restore-Console
    try { Stop-Transcript | Out-Null } catch { }
    exit 1
}

# --- Privilegios -------------------------------------------------------------------
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin -and -not $SinServicios) { Write-Fail "Ejecutar PowerShell como Administrador." }

# --- Contenido del paquete ------------------------------------------------------------
foreach ($f in @("manifest-app.txt", "sync\lfsoft-sync.exe", "sync\config\sync.yaml", "sql\branding.sql", "Mantenimiento-Sync.ps1")) {
    if (-not (Test-Path (Join-Path $pkg $f))) { Write-Fail "Falta $f en el paquete ($pkg)." }
}
$manifest = @()
foreach ($line in [System.IO.File]::ReadAllLines((Join-Path $pkg "manifest-app.txt"))) {
    $l = $line.Trim()
    if (-not $l -or $l.StartsWith('#')) { continue }
    $p = $l.Split('|')
    $file = Join-Path $pkg ("migrations-app\{0}\{1}" -f $p[1], $p[2])
    if ($p[0] -ne "-" -and -not (Test-Path $file)) { Write-Fail "Falta la migracion $($p[1])/$($p[2]) en migrations-app." }
    $manifest += [pscustomobject]@{ Schema = $p[0]; Domain = $p[1]; File = $p[2]; Path = $file }
}

# --- Herramientas de PostgreSQL ------------------------------------------------------
if (-not $PgBin) {
    $candidates = @("C:\Apps\LFSoft\tools\postgresql\bin")
    $candidates += @(Get-ChildItem "C:\Program Files\PostgreSQL\*\bin" -Directory -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | ForEach-Object { $_.FullName })
    # Solo ejecutables reales: un alias o una funcion "psql" no tiene ruta.
    $onPath = Get-Command psql -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($onPath -and $onPath.Source) { $candidates += (Split-Path $onPath.Source) }
    $PgBin = $candidates | Where-Object { $_ -and (Test-Path (Join-Path $_ "psql.exe")) -and (Test-Path (Join-Path $_ "pg_dump.exe")) } | Select-Object -First 1
}
if (-not $PgBin) { Write-Fail "No se encontraron psql.exe y pg_dump.exe. Pasar la carpeta bin con -PgBin." }
$psql = Join-Path $PgBin "psql.exe"
$pgDump = Join-Path $PgBin "pg_dump.exe"
$pgRestore = Join-Path $PgBin "pg_restore.exe"

# --- Configuracion del sync -----------------------------------------------------------
$installedYaml = Join-Path $SyncInstallDir "config\sync.yaml"
$pkgYaml = Join-Path $pkg "sync\config\sync.yaml"
$yamlForSettings = if (Test-Path $installedYaml) { $installedYaml } else { $pkgYaml }
$yamlText = [System.IO.File]::ReadAllText($yamlForSettings)
function Get-YamlValue([string]$text, [string]$key) {
    if ($text -match "(?m)^\s*$key\s*:\s*[`"']?([^`"'\r\n]+)[`"']?") { return $Matches[1].Trim() }
    return ""
}
if (-not $DatabaseUrl) { $DatabaseUrl = Get-YamlValue $yamlText 'pg_dsn' }
if (-not $DatabaseUrl) { Write-Fail "No hay pg_dsn en $yamlForSettings. Pasar -DatabaseUrl." }
if ($DatabaseUrl -notmatch '^postgres(ql)?://' -and $DatabaseUrl -notmatch '=') { Write-Fail "-DatabaseUrl no es una conexion valida: '$DatabaseUrl'. Ejemplo: postgres://postgres:CLAVE@localhost:5432/lfsoft_hidramic" }
if (-not $MdbPath) { $MdbPath = (Get-YamlValue $yamlText 'mdb_base_path') -replace '\\\\', '\' }

# psql no entiende los parametros propios de pgx (search_path, pool_*).
function ConvertTo-LibpqConn([string]$url) {
    $keep = @('sslmode', 'sslrootcert', 'sslcert', 'sslkey', 'connect_timeout', 'application_name', 'host', 'port', 'user', 'password', 'dbname')
    $parts = $url.Split('?', 2)
    if ($parts.Count -eq 1) { return $url }
    $kept = @($parts[1].Split('&') | Where-Object { $keep -contains ($_.Split('=', 2)[0]) })
    if ($kept.Count -eq 0) { return $parts[0] }
    return $parts[0] + '?' + ($kept -join '&')
}
$conn = ConvertTo-LibpqConn $DatabaseUrl
$env:PGCLIENTENCODING = "UTF8"
$env:PGOPTIONS = "-c client_min_messages=warning"

function Invoke-Native([string]$exe, [string[]]$arguments) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    $out = & $exe @arguments 2>&1 | ForEach-Object { "$_" }
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    return [pscustomobject]@{ Code = $code; Output = ($out -join "`n") }
}

# El SQL va por archivo: PS 5.1 no escapa bien las comillas dobles hacia un exe nativo.
function Invoke-Sql([string]$sql, [switch]$Scalar) {
    $tmp = [System.IO.Path]::GetTempFileName()
    [System.IO.File]::WriteAllText($tmp, $sql, $utf8)
    $a = @('-X', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', $tmp)
    if ($Scalar) { $a = @('-qtA') + $a }
    $r = Invoke-Native $psql $a
    Remove-Item $tmp -ErrorAction SilentlyContinue
    if ($r.Code -ne 0) { Write-Fail "Fallo la consulta:`n$sql`n$($r.Output)" }
    return $r.Output.Trim()
}

# --- Servicios -----------------------------------------------------------------------------
function Get-NssmBackends {
    Get-ChildItem "HKLM:\SYSTEM\CurrentControlSet\Services" -ErrorAction SilentlyContinue | ForEach-Object {
        $p = Get-ItemProperty (Join-Path $_.PSPath "Parameters") -ErrorAction SilentlyContinue
        if ($p -and $p.Application -and $p.AppEnvironmentExtra -and ($p.Application -notmatch 'sync')) {
            $dbLine = @($p.AppEnvironmentExtra) | Where-Object { $_ -like "DATABASE_URL=*" } | Select-Object -First 1
            $portLine = @($p.AppEnvironmentExtra) | Where-Object { $_ -like "PORT=*" } | Select-Object -First 1
            if ($dbLine) {
                [pscustomobject]@{
                    Name        = $_.PSChildName
                    DatabaseUrl = $dbLine.Substring(13)
                    Port        = if ($portLine) { $portLine.Substring(5) } else { "" }
                }
            }
        }
    }
}

# ====================================================================================
Write-Step "1/10 Verificaciones previas"
$who = Invoke-Sql "SELECT current_database() || '|' || current_user || '|' || current_setting('server_version_num') || '|' || pg_size_pretty(pg_database_size(current_database())) || '|' || pg_database_size(current_database())" -Scalar
$w = $who.Split('|')
$dbName = $w[0]; $pgVersion = [int]$w[2]; $dbSizeBytes = [int64]$w[4]
Write-Ok "Conexion OK: base $dbName, usuario $($w[1]), PostgreSQL $pgVersion, tamano $($w[3])"
if ($dbName -ne $DatabaseName) { Write-Fail "La conexion apunta a la base '$dbName' y se esperaba '$DatabaseName'. Revisar -DatabaseUrl o -DatabaseName." }
$isSuper = Invoke-Sql "SELECT rolsuper FROM pg_roles WHERE rolname = current_user" -Scalar
if ($isSuper -ne 't') { Write-Warn "El usuario no es superusuario: si no es dueno de todos los schemas, el borrado va a fallar." }

$schemasNow = @((Invoke-Sql "SELECT COALESCE(string_agg(nspname, ',' ORDER BY nspname), '') FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'" -Scalar).Split(',') | Where-Object { $_ })
Write-Ok "Schemas actuales: $($schemasNow -join ', ')"
$tablesNow = Invoke-Sql "SELECT COUNT(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema')" -Scalar
Write-Ok "Tablas actuales: $tablesNow"

# Entidades del sync del paquete: de ahi salen los .mdb necesarios.
$entityFiles = @(Get-ChildItem (Join-Path $pkg "sync\config\entities\*.yaml"))
$mdbs = @()
foreach ($ef in $entityFiles) {
    $t = [System.IO.File]::ReadAllText($ef.FullName)
    if ($t -match '(?m)^enabled:\s*true' -and $t -match '(?m)^\s+mdb:\s*(\S+)') { $mdbs += $Matches[1] }
}
$mdbs = @($mdbs | Select-Object -Unique)
Write-Ok "Entidades del sync: $($entityFiles.Count) archivos; .mdb usados: $($mdbs -join ', ')"
if (-not $MdbPath -or -not (Test-Path $MdbPath)) { Write-Fail "No existe la carpeta de los .mdb '$MdbPath' (mdb_base_path de sync.yaml o -MdbPath)." }
$mdbBytes = 0
foreach ($m in $mdbs) {
    $mp = Join-Path $MdbPath $m
    if (-not (Test-Path $mp)) { Write-Fail "Falta ${mp}: el sync no podria arrancar." }
    $mdbBytes += (Get-Item $mp).Length
}
Write-Ok "Carpeta de los .mdb: $MdbPath (todos presentes, $([math]::Round($mdbBytes / 1MB)) MB)"

$backends = @(Get-NssmBackends | Where-Object { $_.DatabaseUrl -match "/$([regex]::Escape($DatabaseName))(\?|$)" })
if ($BackendServiceName) { $backends = @(Get-NssmBackends | Where-Object { $_.Name -eq $BackendServiceName }) }
if ($backends.Count -eq 0) {
    Write-Warn "No se encontro el servicio NSSM del backend que usa $DatabaseName. Detenerlo a mano antes de seguir (o pasar -BackendServiceName)."
} else {
    foreach ($b in $backends) {
        $svc = Get-Service $b.Name -ErrorAction SilentlyContinue
        Write-Ok "Backend: servicio $($b.Name) ($($svc.Status)), puerto $($b.Port)"
        $sp = ""
        if ($b.DatabaseUrl -match 'search_path=([^&]+)') { $sp = [uri]::UnescapeDataString($Matches[1]) }
        $missingSp = @($appSchemas | Where-Object { ($sp -split ',') -notcontains $_ })
        if ($missingSp.Count -gt 0) {
            Write-Warn "El DATABASE_URL de $($b.Name) no incluye en search_path: $($missingSp -join ', '). Sin eso el backend da 500 en esos modulos. Valor actual: '$sp'"
        }
    }
}

New-Item -ItemType Directory -Force $BackupRoot | Out-Null
$drive = (Get-Item $BackupRoot).PSDrive
$needed = [int64]($dbSizeBytes * 1.5) + $(if ($SinBackupMdb) { 0 } else { $mdbBytes }) + 200MB
if ($drive.Free -lt $needed) { Write-Fail "Espacio insuficiente en $($drive.Name): para el backup ($([math]::Round($needed / 1MB)) MB necesarios, $([math]::Round($drive.Free / 1MB)) MB libres)." }
Write-Ok "Backup en $BackupRoot (libres $([math]::Round($drive.Free / 1GB, 1)) GB)"

$syncSvc = Get-Service $SyncServiceName -ErrorAction SilentlyContinue
Write-Ok "Servicio del sync: $(if ($syncSvc) { "$SyncServiceName ($($syncSvc.Status))" } else { 'no instalado' })"
Write-Ok "Migraciones de la app a aplicar: $($manifest.Count)"

if ($SoloVerificar) {
    Write-Host ""
    Write-Host "Verificacion terminada. No se modifico nada." -ForegroundColor Cyan
    Restore-Console
    try { Stop-Transcript | Out-Null } catch { }
    exit 0
}

if ($DesdeCargaInicial) {
    Write-Step "Retomando desde la carga inicial (-DesdeCargaInicial)"
    Write-Host "  No se hace backup ni se borra ni se migra: se reinstala el sync, se corre la carga inicial"
    Write-Host "  y se arrancan los servicios."
    $appTables = Invoke-Sql "SELECT COUNT(*) FROM pg_tables WHERE schemaname IN ('articulos','sales','utilities','seguridad','settings','precios')" -Scalar
    if ([int]$appTables -lt 50) { Write-Fail "La base no tiene las tablas de la app ($appTables): correr el reset completo, sin -DesdeCargaInicial." }
} else {
# ====================================================================================
Write-Step "2/10 Confirmacion"
Write-Host "  Se van a BORRAR todos los schemas y tablas de '$dbName' ($tablesNow tablas)." -ForegroundColor Red
Write-Host "  Se conservan los usuarios ADMIN (con su contrasena) y los permisos del rol ADMIN. El resto"
Write-Host "  de los usuarios, roles y configuraciones de la app vuelve a los valores iniciales. Los datos"
Write-Host "  se vuelven a cargar desde los .mdb. Antes se hace un backup completo."
if (-not $Forzar) {
    $answer = Read-Host "  Escribir el nombre de la base para confirmar"
    if ($answer -ne $dbName) { Write-Fail "Confirmacion incorrecta. No se modifico nada." }
}
}

# ====================================================================================
Write-Step "3/10 Deteniendo servicios"
$backendWasRunning = @{}
if ($SinServicios) {
    $running = @(@($backends | ForEach-Object { $_.Name }) + @($SyncServiceName) | Where-Object { (Get-Service $_ -ErrorAction SilentlyContinue).Status -eq "Running" })
    if ($running.Count -gt 0) { Write-Fail "Con -SinServicios hay que detener antes: $($running -join ", ")." }
    Write-Ok "Omitido (-SinServicios)"; $backends = @(); $syncSvc = $null
}
foreach ($b in $backends) {
    $svc = Get-Service $b.Name -ErrorAction SilentlyContinue
    if ($svc) {
        $backendWasRunning[$b.Name] = ($svc.Status -eq 'Running')
        if ($svc.Status -ne 'Stopped') { Stop-Service $b.Name -Force; Write-Ok "Detenido $($b.Name)" }
    }
}
if ($syncSvc -and $syncSvc.Status -ne 'Stopped') { Stop-Service $SyncServiceName -Force; Write-Ok "Detenido $SyncServiceName" }
Start-Sleep -Seconds 3

if (-not $DesdeCargaInicial) {
# ====================================================================================
Write-Step "4/10 Backup"
$script:backupDir = Join-Path $BackupRoot "reset_$stamp"
New-Item -ItemType Directory -Force $script:backupDir | Out-Null
$dumpFile = Join-Path $script:backupDir "$dbName.dump"
Write-Host "  pg_dump de $dbName..."
$r = Invoke-Native $pgDump @('-d', $conn, '-Fc', '-Z', '6', '-f', $dumpFile)
if ($r.Code -ne 0) { Write-Fail "Fallo pg_dump:`n$($r.Output)" }
$r = Invoke-Native $pgRestore @('-l', $dumpFile)
if ($r.Code -ne 0 -or ($r.Output -split "`n").Count -lt 10) { Write-Fail "El backup no se pudo verificar con pg_restore -l:`n$($r.Output)" }
Write-Ok "Base: $dumpFile ($([math]::Round((Get-Item $dumpFile).Length / 1MB, 1)) MB, $((($r.Output -split "`n") | Where-Object { $_ -notmatch '^;' }).Count) objetos)"

# branding (nombre y logo): la tabla la creo la consolidacion y no ninguna migracion.
$hasBranding = Invoke-Sql "SELECT to_regclass('settings.branding') IS NOT NULL" -Scalar
$brandingFile = Join-Path $script:backupDir "branding.sql"
if ($hasBranding -eq 't') {
    $r = Invoke-Native $pgDump @('-d', $conn, '-t', 'settings.branding', '--no-owner', '--no-privileges', '-f', $brandingFile)
    if ($r.Code -ne 0) { Write-Fail "No se pudo respaldar settings.branding:`n$($r.Output)" }
    Write-Ok "settings.branding respaldada aparte (se restaura despues de las migraciones)"
}

# Usuario(s) administrador(es) con sus permisos: se vuelven a cargar despues de las migraciones,
# con la misma contrasena. Rol ADMIN = 1 (rol de sistema, seguridad V028).
$adminFile = Join-Path $script:backupDir "admin.sql"
$adminSql = New-Object System.Text.StringBuilder
$adminQueries = @(
    @("seguridad.users", @"
SELECT COALESCE(string_agg(format(
  'INSERT INTO seguridad.users ("user_username", "user_passwordHash", "user_email", "user_fullName", "user_role", "user_isActive") VALUES (%L, %L, %L, %L, 1, %L) ON CONFLICT ("user_username") DO UPDATE SET "user_passwordHash" = EXCLUDED."user_passwordHash", "user_email" = EXCLUDED."user_email", "user_fullName" = EXCLUDED."user_fullName", "user_role" = 1, "user_isActive" = EXCLUDED."user_isActive";',
  "user_username", "user_passwordHash", "user_email", "user_fullName", "user_isActive"), E'\n'), '')
FROM seguridad.users WHERE "user_role" = 1
"@),
    @("seguridad.role_permissions", @"
SELECT COALESCE(string_agg(format(
  'INSERT INTO seguridad.role_permissions ("role_permission_userRole", "role_permission_literalKey", "role_permission_caseId", "role_permission_conditions", "role_permission_actions") VALUES (1, %L, %s, %L, %L) ON CONFLICT ("role_permission_userRole", "role_permission_literalKey", "role_permission_caseId") DO UPDATE SET "role_permission_conditions" = EXCLUDED."role_permission_conditions", "role_permission_actions" = EXCLUDED."role_permission_actions";',
  "role_permission_literalKey", "role_permission_caseId", "role_permission_conditions", "role_permission_actions"), E'\n'), '')
FROM seguridad.role_permissions WHERE "role_permission_userRole" = 1
"@),
    @("seguridad.role_hidden_fields", @"
SELECT COALESCE(string_agg(format(
  'INSERT INTO seguridad.role_hidden_fields ("role_hidden_field_userRole", "role_hidden_field_literalKey", "role_hidden_field_hiddenFields", "role_hidden_field_isActive") VALUES (1, %L, %L, %L) ON CONFLICT ("role_hidden_field_userRole", "role_hidden_field_literalKey") DO UPDATE SET "role_hidden_field_hiddenFields" = EXCLUDED."role_hidden_field_hiddenFields", "role_hidden_field_isActive" = EXCLUDED."role_hidden_field_isActive";',
  "role_hidden_field_literalKey", "role_hidden_field_hiddenFields", "role_hidden_field_isActive"), E'\n'), '')
FROM seguridad.role_hidden_fields WHERE "role_hidden_field_userRole" = 1
"@)
)
$adminUsers = 0
foreach ($aq in $adminQueries) {
    if ((Invoke-Sql "SELECT to_regclass('$($aq[0])') IS NOT NULL" -Scalar) -ne 't') { continue }
    $out = Invoke-Sql $aq[1] -Scalar
    if ($out) { [void]$adminSql.AppendLine($out) }
    if ($aq[0] -eq 'seguridad.users') { $adminUsers = @($out -split "`n" | Where-Object { $_ }).Count }
}
[System.IO.File]::WriteAllText($adminFile, $adminSql.ToString(), $utf8)
Write-Ok "Administradores respaldados aparte con sus permisos ($adminUsers usuario(s) con rol ADMIN)"

if (Test-Path $SyncInstallDir) {
    $syncBackup = Join-Path $script:backupDir "sync-instalado"
    New-Item -ItemType Directory -Force $syncBackup | Out-Null
    Get-ChildItem $SyncInstallDir -Exclude 'logs' | Copy-Item -Destination $syncBackup -Recurse -Force
    Write-Ok "Instalacion anterior del sync: $syncBackup"
}

if (-not $SinBackupMdb) {
    $mdbBackup = Join-Path $script:backupDir "mdb"
    New-Item -ItemType Directory -Force $mdbBackup | Out-Null
    foreach ($m in $mdbs) {
        Copy-Item (Join-Path $MdbPath $m) $mdbBackup -Force
    }
    Write-Ok ".mdb usados por el sync: $mdbBackup"
}
[System.IO.File]::WriteAllText((Join-Path $script:backupDir "LEEME.txt"),
    "Backup previo al reset de $dbName ($stamp).`r`nBase: $dbName.dump (pg_dump -Fc). Restaurar con Restore-LFSoft.ps1 -Backup `"$script:backupDir`".`r`n", $utf8)

# ====================================================================================
Write-Step "5/10 Borrando schemas"
$public = if ($pgVersion -ge 150000) {
    "CREATE SCHEMA public AUTHORIZATION pg_database_owner; GRANT USAGE ON SCHEMA public TO PUBLIC;"
} else {
    "CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO PUBLIC;"
}
$null = Invoke-Sql @"
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid();
DO `$`$
DECLARE s RECORD;
BEGIN
    FOR s IN SELECT nspname FROM pg_namespace
             WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
    LOOP
        EXECUTE format('DROP SCHEMA %I CASCADE', s.nspname);
    END LOOP;
END `$`$;
$public
$(($appSchemas | ForEach-Object { "CREATE SCHEMA $_;" }) -join "`n")
CREATE TABLE $tracking (
    "schema_name" TEXT        NOT NULL,
    "version"     TEXT        NOT NULL,
    "file_name"   TEXT        NOT NULL,
    "checksum"    TEXT        NOT NULL,
    "status"      TEXT        NOT NULL,
    "applied_at"  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("schema_name", "version")
);
SELECT 1;
"@
$left = Invoke-Sql "SELECT COUNT(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema','public')" -Scalar
if ($left -ne '0') { Write-Fail "Quedaron $left tablas despues del borrado." }
Write-Ok "Base vacia. Schemas creados: public, $($appSchemas -join ', ')"

# ====================================================================================
Write-Step "6/10 Migraciones de la app ($($manifest.Count))"
$applied = 0
foreach ($m in $manifest) {
    $version = $m.File.Split('_')[0]
    $hash = if (Test-Path $m.Path) { (Get-FileHash $m.Path -Algorithm SHA256).Hash.ToLower() } else { "" }
    $label = "{0,-10} {1}" -f $m.Domain, $m.File
    if ($m.Schema -eq '-') {
        $null = Invoke-Sql "INSERT INTO $tracking (schema_name, version, file_name, checksum, status) VALUES ('$($m.Domain)', '$version', '$($m.File)', '$hash', 'omitida') ON CONFLICT DO NOTHING; SELECT 1;"
        Write-Host "  [omitida]  $label" -ForegroundColor DarkGray
        continue
    }
    $bytes = [System.IO.File]::ReadAllBytes($m.Path)
    if ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) { $bytes = $bytes[3..($bytes.Length - 1)] }
    $body = $utf8.GetString($bytes)
    # Las semillas grandes manejan su propia transaccion (BEGIN/COMMIT): esas van sin envolver.
    $ownTx = $body -match '(?im)^\s*(BEGIN|COMMIT)\s*;'
    $sql = "SET search_path TO $($m.Schema), public;`n" + $body +
        "`n;`nRESET search_path;`nINSERT INTO $tracking (schema_name, version, file_name, checksum, status) VALUES ('$($m.Domain)', '$version', '$($m.File)', '$hash', 'aplicada') ON CONFLICT DO NOTHING;`n"
    $tmp = [System.IO.Path]::GetTempFileName()
    [System.IO.File]::WriteAllText($tmp, $sql, $utf8)
    $a = @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', $tmp)
    if (-not $ownTx) { $a = @('--single-transaction') + $a }
    $r = Invoke-Native $psql $a
    Remove-Item $tmp -ErrorAction SilentlyContinue
    if ($r.Code -ne 0) {
        $err = Join-Path $pkg ("logs\error_{0}_{1}.log" -f $m.Domain, $version)
        [System.IO.File]::WriteAllText($err, $r.Output, $utf8)
        try { Write-Host $r.Output } catch { }
        Write-Fail "Fallo la migracion $($m.Domain)\$($m.File). Detalle: $err"
    }
    $applied++
    if ($applied % 25 -eq 0) { Write-Host "  ... $applied aplicadas (ultima: $label)" }
}
Write-Ok "$applied migraciones aplicadas y registradas en $tracking"

# ====================================================================================
Write-Step "7/10 Administrador y branding"
if ((Get-Item $adminFile).Length -gt 0) {
    $r = Invoke-Native $psql @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '--single-transaction', '-d', $conn, '-f', $adminFile)
    if ($r.Code -ne 0) { Write-Fail "No se pudieron restaurar los administradores:`n$($r.Output)" }
    Write-Ok "Usuarios ADMIN restaurados con su contrasena, permisos y campos ocultos del rol"
} else {
    Write-Warn "La base anterior no tenia usuarios ADMIN: queda el administrador inicial de las migraciones."
}
if (Test-Path $brandingFile) {
    $r = Invoke-Native $psql @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', $brandingFile)
    if ($r.Code -ne 0) { Write-Fail "No se pudo restaurar settings.branding:`n$($r.Output)" }
    Write-Ok "settings.branding restaurada desde el backup (nombre y logo de la empresa)"
} else {
    $r = Invoke-Native $psql @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', (Join-Path $pkg "sql\branding.sql"))
    if ($r.Code -ne 0) { Write-Fail "No se pudo crear settings.branding:`n$($r.Output)" }
    Write-Warn "La base no tenia settings.branding: se creo vacia (la app usa el logo generico)."
}
# Columnas visibles de las grillas: un script por grilla. Un error no corta el reset (la grilla
# queda con todas sus columnas y se puede reintentar con Aplicar-Grillas.ps1).
if (-not $SinGrillas) {
    $grillas = @(Get-ChildItem (Join-Path $pkg "sql\grillas\*.sql") -ErrorAction SilentlyContinue | Sort-Object Name)
    $grillasFail = 0
    foreach ($g in $grillas) {
        $r = Invoke-Native $psql @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', $g.FullName)
        if ($r.Code -ne 0) { $grillasFail++; Write-Warn "Grilla $($g.BaseName):"; Write-Safe $r.Output }
        elseif ($r.Output) { Write-Safe $r.Output }
    }
    if ($grillas.Count -eq 0) { Write-Warn "No hay scripts en sql\grillas: las grillas quedan con todas sus columnas." }
    elseif ($grillasFail -gt 0) { Write-Warn "$grillasFail grilla(s) no se pudieron aplicar: reintentar con .\Aplicar-Grillas.ps1" }
    else { Write-Ok "Columnas relevantes y engranaje de configuracion en $($grillas.Count) grillas (sql\grillas)" }
}
# Campos ID ocultos en filtros y forms, para todos los roles. Va despues de restaurar los campos
# ocultos de ADMIN para no pisarlos. Un error no corta el reset (reintentar con Ocultar-CamposId.ps1).
$idsFile = Join-Path $pkg "sql\campos-id-ocultos.sql"
if (Test-Path $idsFile) {
    $r = Invoke-Native $psql @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', $idsFile)
    if ($r.Code -ne 0) { Write-Warn "Campos ID ocultos: reintentar con .\Ocultar-CamposId.ps1"; Write-Safe $r.Output }
    else { Write-Ok "Campos ID ocultos en filtros y forms (sql\campos-id-ocultos.sql)" }
}

}

# ====================================================================================
Write-Step "8/10 Instalando el sync en $SyncInstallDir"
$existingSvc = Get-Service $SyncServiceName -ErrorAction SilentlyContinue
if ($existingSvc -and -not $SinServicios) {
    sc.exe delete $SyncServiceName | Out-Null
    Start-Sleep -Seconds 2
}
foreach ($d in @("", "config", "config\entities", "migrations", "logs", "scripts")) {
    New-Item -ItemType Directory -Force (Join-Path $SyncInstallDir $d) | Out-Null
}
Copy-Item (Join-Path $pkg "sync\lfsoft-sync.exe") $SyncInstallDir -Force
if (-not (Test-Path $installedYaml)) {
    Copy-Item $pkgYaml $installedYaml -Force
    Write-Ok "config\sync.yaml nuevo (del paquete)"
} else {
    Write-Ok "config\sync.yaml existente conservado (conexion y carpeta de .mdb del servidor)"
}
# Las entidades se copian a <instalacion>\config\entities: entities_dir tiene que apuntar ahi.
$entitiesDir = Join-Path $SyncInstallDir "config\entities"
$cfgText = [System.IO.File]::ReadAllText($installedYaml)
$currentDir = (Get-YamlValue $cfgText 'entities_dir') -replace '\\\\', '\'
if ($currentDir -ne $entitiesDir) {
    $line = "entities_dir: '$entitiesDir'"
    if ($cfgText -match '(?m)^\s*entities_dir\s*:.*$') { $cfgText = $cfgText -replace '(?m)^\s*entities_dir\s*:.*$', $line.Replace('$', '$$') }
    else { $cfgText = $cfgText.TrimEnd() + "`r`n" + $line + "`r`n" }
    [System.IO.File]::WriteAllText($installedYaml, $cfgText, $utf8)
    Write-Ok "entities_dir de sync.yaml corregido: '$currentDir' -> '$entitiesDir'"
}
# Las entidades se reemplazan enteras: una vieja que quede suelta seguiria sincronizando.
Get-ChildItem (Join-Path $SyncInstallDir "config\entities\*.yaml") -ErrorAction SilentlyContinue | Remove-Item -Force
Copy-Item (Join-Path $pkg "sync\config\entities\*.yaml") (Join-Path $SyncInstallDir "config\entities") -Force
Get-ChildItem (Join-Path $SyncInstallDir "migrations\*.sql") -ErrorAction SilentlyContinue | Remove-Item -Force
Copy-Item (Join-Path $pkg "sync\migrations\*.sql") (Join-Path $SyncInstallDir "migrations") -Force
Copy-Item (Join-Path $pkg "Mantenimiento-Sync.ps1") (Join-Path $SyncInstallDir "scripts") -Force
Write-Ok "Exe, $((Get-ChildItem (Join-Path $SyncInstallDir 'config\entities\*.yaml')).Count) entidades y migraciones copiados"

foreach ($mig in (Get-ChildItem (Join-Path $SyncInstallDir "migrations\*.sql") | Sort-Object Name)) {
    $r = Invoke-Native $psql @('-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $conn, '-f', $mig.FullName)
    if ($r.Code -ne 0) { Write-Fail "Fallo la migracion del sync $($mig.Name):`n$($r.Output)" }
}
$trg = Invoke-Sql "SELECT COUNT(*) FROM pg_trigger WHERE tgname LIKE 'trg\_%\_sync' AND NOT tgisinternal" -Scalar
Write-Ok "Migraciones del sync aplicadas ($trg triggers de sincronizacion PG -> Access)"

# ====================================================================================
Write-Step "9/10 Carga inicial Access -> PG"
Write-Host "  Puede tardar varios minutos (presupuestos es la entidad mas grande)."
$exe = Join-Path $SyncInstallDir "lfsoft-sync.exe"
$syncCfg = $installedYaml
$prev = $ErrorActionPreference
$ErrorActionPreference = "Continue"
# stderr lleva el log JSON (tambien queda en logs\sync.log); stdout, el resumen por entidad, va a un
# archivo y se muestra despues: si la consola no puede escribir algo, la carga no se interrumpe.
$summary = Join-Path $pkg "logs\carga-inicial_$stamp.txt"
& $exe --config $syncCfg --initial-sync 2>$null > $summary
$initCode = $LASTEXITCODE
$ErrorActionPreference = $prev
foreach ($line in [System.IO.File]::ReadAllLines($summary)) { Write-Safe "  $line" }
Write-Ok "Resumen guardado en $summary"
if ($initCode -eq 2) { Write-Fail "La carga inicial no pudo correr (conexion o configuracion). Ver $SyncInstallDir\logs\sync.log." }
if ($initCode -eq 1) {
    Write-Warn "Algunas filas no se pudieron cargar (marcadas arriba). El resto quedo cargado y el servicio las reintenta en cada barrido. Detalle en $SyncInstallDir\logs\sync.log."
} else {
    Write-Ok "Carga inicial completa sin errores"
}
$null = Invoke-Sql "ANALYZE; SELECT 1;"

# ====================================================================================
Write-Step "10/10 Servicios y tareas programadas"
if ($SinServicios) {
    Write-Ok "Omitido (-SinServicios): instalar el servicio con install-server.ps1 o volver a correr sin -SinServicios"
} else {
$binPath = "`"$exe`" --config `"$syncCfg`""
New-Service -Name $SyncServiceName -BinaryPathName $binPath -DisplayName "LFSoft Sync Service" -StartupType Automatic `
    -Description "Sincronizacion Access <-> PostgreSQL (LFSoft). Barridos periodicos por entidad segun config\entities." | Out-Null
sc.exe failure $SyncServiceName reset= 86400 actions= restart/5000/restart/10000/restart/30000 | Out-Null
Start-Service $SyncServiceName
Start-Sleep -Seconds 5
$svc = Get-Service $SyncServiceName
if ($svc.Status -ne 'Running') { Write-Warn "El servicio $SyncServiceName no quedo corriendo ($($svc.Status)). Probar en consola: & `"$exe`" --config `"$syncCfg`"" }
else {
    $port = Get-YamlValue ([System.IO.File]::ReadAllText($syncCfg)) 'sync_port'
    if (-not $port) { $port = "9090" }
    try {
        $h = Invoke-RestMethod "http://127.0.0.1:$port/sync/health" -TimeoutSec 10
        Write-Ok "Servicio $SyncServiceName corriendo (health: $($h.status), puerto $port)"
    } catch { Write-Warn "El servicio corre pero /sync/health no respondio todavia en el puerto $port." }
}

# Mantenimiento nocturno: reinicia el sync (el primer barrido despues de arrancar compara todas
# las filas de todas las entidades), rota sync.log y purga sync.sync_log / sync.pg_changes.
$taskName = "LFSoft Sync - Mantenimiento"
$maint = Join-Path $SyncInstallDir "scripts\Mantenimiento-Sync.ps1"
$action = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$maint`" -SyncInstallDir `"$SyncInstallDir`" -ServiceName $SyncServiceName -PgBin `"$PgBin`""
$trigger = New-ScheduledTaskTrigger -Daily -At $MaintenanceTime
$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Hours 1) -StartWhenAvailable
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
Write-Ok "Tarea programada '$taskName' todos los dias a las $MaintenanceTime"

foreach ($b in $backends) {
    if ($backendWasRunning[$b.Name] -or $DesdeCargaInicial) {
        Start-Service $b.Name
        Start-Sleep -Seconds 3
        $port = if ($b.Port) { $b.Port } else { "3000" }
        try {
            $null = Invoke-WebRequest "http://127.0.0.1:$port/health" -UseBasicParsing -TimeoutSec 10
            Write-Ok "Backend $($b.Name) arrancado (/health OK en el puerto $port)"
        } catch { Write-Warn "Backend $($b.Name) arrancado, pero /health no respondio en el puerto $port." }
    }
}
}

# ====================================================================================
Write-Host ""
Write-Host "=== Reset terminado ===" -ForegroundColor Cyan
if ($script:backupDir) { Write-Host "  Backup:           $script:backupDir" }
Write-Host "  Log de este reset: $logFile"
Write-Host "  Log del sync:     $SyncInstallDir\logs\sync.log"
Write-Host ""
Write-Host "  Proximos pasos:" -ForegroundColor Yellow
Write-Host "   - Entrar a la app con el usuario ADMIN de siempre y volver a dar de alta los demas usuarios"
Write-Host "     y sus permisos (estan en el backup si hace falta recuperarlos)."
Write-Host "   - Revisar en la app: articulos, clientes, presupuestos, rendiciones, listas de precios."
Write-Host "   - Seguir el sync: Get-Content `"$SyncInstallDir\logs\sync.log`" -Tail 50 -Wait"
Restore-Console
try { Stop-Transcript | Out-Null } catch { }
