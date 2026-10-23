<#
.SYNOPSIS
    Instala el servicio LFSoft-sync en el servidor Windows.
.DESCRIPTION
    Ejecutar como Administrador en el servidor de destino.
    Copia archivos a C:\Apps\LFSoft\tools\sync, ejecuta migraciones SQL e instala
    el servicio de Windows.
.EXAMPLE
    .\install-server.ps1
    .\install-server.ps1 -InstallDir "D:\LFSoft\sync" -PsqlPath "C:\Apps\LFSoft\tools\postgresql\bin\psql.exe"
#>
param(
    [string]$InstallDir = "C:\Apps\LFSoft\tools\sync",
    [string]$PsqlPath = "",
    [string]$ServiceName = "LFSoftSync",
    [string]$ServiceDisplayName = "LFSoft Sync Service",
    [switch]$SkipMigrations,
    [switch]$SkipService,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"
$scriptDir = $PSScriptRoot

# --- Verificar privilegios ---
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "ERROR: Ejecutar como Administrador." -ForegroundColor Red
    exit 1
}

# --- Desinstalar ---
if ($Uninstall) {
    Write-Host "=== Desinstalando $ServiceName ===" -ForegroundColor Cyan
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($svc) {
        if ($svc.Status -eq "Running") {
            Write-Host "Deteniendo servicio..."
            Stop-Service -Name $ServiceName -Force
            Start-Sleep -Seconds 2
        }
        Write-Host "Eliminando servicio..."
        sc.exe delete $ServiceName | Out-Null
        Write-Host "Servicio eliminado." -ForegroundColor Green
    } else {
        Write-Host "Servicio no encontrado." -ForegroundColor DarkYellow
    }
    Write-Host "Los archivos en $InstallDir NO se eliminaron."
    exit 0
}

Write-Host "=== LFSoft-sync: Instalacion en servidor ===" -ForegroundColor Cyan
Write-Host "Directorio de instalacion: $InstallDir"
Write-Host ""

# --- Verificar archivos de deploy ---
$requiredFiles = @(
    "lfsoft-sync.exe",
    "config\sync.yaml"
)
foreach ($f in $requiredFiles) {
    if (-not (Test-Path (Join-Path $scriptDir $f))) {
        Write-Host "ERROR: No se encuentra $f en $scriptDir" -ForegroundColor Red
        Write-Host "Ejecutar primero deploy-build.ps1 en la maquina de desarrollo."
        exit 1
    }
}

# --- Verificar que los .mdb existen ---
$syncYaml = Get-Content "$scriptDir\config\sync.yaml" -Raw
if ($syncYaml -match 'mdb_base_path:\s*"([^"]+)"') {
    $mdbPath = $Matches[1] -replace '\\\\', '\'
    Write-Host "Verificando acceso a .mdb en: $mdbPath"
    if (Test-Path $mdbPath) {
        $mdbFiles = Get-ChildItem "$mdbPath\*.mdb" -ErrorAction SilentlyContinue
        Write-Host "  Encontrados: $($mdbFiles.Count) archivos .mdb" -ForegroundColor Green
        foreach ($m in $mdbFiles) {
            Write-Host "    $($m.Name) ($([math]::Round($m.Length / 1MB, 1)) MB)"
        }
    } else {
        Write-Host "  ADVERTENCIA: La carpeta $mdbPath no existe!" -ForegroundColor Red
        Write-Host "  Verificar mdb_base_path en config\sync.yaml"
        $continue = Read-Host "  Continuar de todos modos? (s/n)"
        if ($continue -ne "s") { exit 1 }
    }
}

# --- Detener y eliminar servicio existente ---
$existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existingSvc) {
    if ($existingSvc.Status -eq "Running") {
        Write-Host "`nDeteniendo servicio existente..."
        Stop-Service -Name $ServiceName -Force
        Start-Sleep -Seconds 3
    }
    Write-Host "Eliminando servicio existente..."
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 2
    Write-Host "  Servicio anterior eliminado." -ForegroundColor Green
}

# --- 1. Crear estructura de directorios ---
Write-Host "`n[1/4] Creando directorios..." -ForegroundColor Yellow
$dirs = @(
    $InstallDir,
    "$InstallDir\config\entities",
    "$InstallDir\migrations",
    "$InstallDir\migrations-app",
    "$InstallDir\logs"
)
foreach ($d in $dirs) {
    New-Item -ItemType Directory -Force $d | Out-Null
}
Write-Host "  OK" -ForegroundColor Green

# --- 2. Copiar archivos ---
Write-Host "`n[2/4] Copiando archivos..." -ForegroundColor Yellow
Copy-Item "$scriptDir\lfsoft-sync.exe" "$InstallDir\" -Force
Write-Host "  + lfsoft-sync.exe"

Copy-Item "$scriptDir\config\sync.yaml" "$InstallDir\config\" -Force
Write-Host "  + config\sync.yaml"

$entityFiles = Get-ChildItem "$scriptDir\config\entities\*.yaml" -ErrorAction SilentlyContinue
foreach ($f in $entityFiles) {
    Copy-Item $f.FullName "$InstallDir\config\entities\" -Force
    Write-Host "  + config\entities\$($f.Name)"
}

$migrationFiles = Get-ChildItem "$scriptDir\migrations\*.sql" -ErrorAction SilentlyContinue
foreach ($f in $migrationFiles) {
    Copy-Item $f.FullName "$InstallDir\migrations\" -Force
    Write-Host "  + migrations\$($f.Name)"
}

$appMigrations = Get-ChildItem "$scriptDir\migrations-app\*.sql" -ErrorAction SilentlyContinue
foreach ($f in $appMigrations) {
    Copy-Item $f.FullName "$InstallDir\migrations-app\" -Force
    Write-Host "  + migrations-app\$($f.Name)"
}

Write-Host "  Archivos copiados." -ForegroundColor Green

# --- 3. Migraciones SQL ---
if (-not $SkipMigrations) {
    Write-Host "`n[3/4] Ejecutando migraciones SQL..." -ForegroundColor Yellow

    if (-not $PsqlPath) {
        $pgPaths = @(
            "C:\Apps\LFSoft\tools\postgresql\bin\psql.exe",
            "C:\Program Files\PostgreSQL\17\bin\psql.exe",
            "C:\Program Files\PostgreSQL\16\bin\psql.exe",
            "C:\Program Files\PostgreSQL\15\bin\psql.exe",
            "C:\Program Files\PostgreSQL\14\bin\psql.exe",
            "C:\Program Files\PostgreSQL\13\bin\psql.exe",
            "C:\Program Files\PostgreSQL\12\bin\psql.exe"
        )
        foreach ($p in $pgPaths) {
            if (Test-Path $p) {
                $PsqlPath = $p
                break
            }
        }
        $psqlCmd = Get-Command psql -ErrorAction SilentlyContinue
        if (-not $PsqlPath -and $psqlCmd) {
            $PsqlPath = $psqlCmd.Source
        }
    }

    if ($PsqlPath -and (Test-Path $PsqlPath -PathType Container)) {
        $PsqlPath = Join-Path $PsqlPath "psql.exe"
    }

    if (-not $PsqlPath -or -not (Test-Path $PsqlPath)) {
        Write-Host "  ADVERTENCIA: psql no encontrado." -ForegroundColor DarkYellow
        Write-Host "  Ejecutar manualmente las migraciones:"
        Write-Host ""
        Write-Host "  Migraciones del sync (esquema sync):" -ForegroundColor White
        Get-ChildItem "$InstallDir\migrations\*.sql" | Sort-Object Name | ForEach-Object {
            Write-Host "    psql -U postgres -d lfsoft_hidramic -f `"$($_.FullName)`""
        }
        Write-Host ""
        Write-Host "  Migraciones de la app (ALTER IDENTITY):" -ForegroundColor White
        Get-ChildItem "$InstallDir\migrations-app\*.sql" -ErrorAction SilentlyContinue | Sort-Object Name | ForEach-Object {
            Write-Host "    psql -U postgres -d lfsoft_hidramic -f `"$($_.FullName)`""
        }
    } else {
        Write-Host "  psql encontrado: $PsqlPath"

        if ($syncYaml -match 'pg_dsn:\s*"postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)') {
            $pgUser = $Matches[1]
            $pgPass = $Matches[2]
            $pgHost = $Matches[3]
            $pgPort = $Matches[4]
            $pgDb   = $Matches[5]
        } else {
            $pgUser = "postgres"
            $pgPass = "lfsoft"
            $pgHost = "localhost"
            $pgPort = "5432"
            $pgDb   = "lfsoft_hidramic"
        }

        $env:PGPASSWORD = $pgPass
        # Ocultar NOTICE de migraciones idempotentes (ej. "ya existe, omitiendo")
        $env:PGOPTIONS = "-c client_min_messages=warning"
        # En PS 5.1, stderr de un exe nativo con 2>&1 se vuelve error terminante bajo "Stop";
        # el resultado real se controla con $LASTEXITCODE + ON_ERROR_STOP=1.
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = "Continue"

        Write-Host "  Conectando a $pgHost`:$pgPort/$pgDb como $pgUser..."

        $syncMigrations = Get-ChildItem "$InstallDir\migrations\*.sql" | Sort-Object Name
        foreach ($mig in $syncMigrations) {
            Write-Host "  Ejecutando $($mig.Name)..." -NoNewline
            $output = & $PsqlPath -h $pgHost -p $pgPort -U $pgUser -d $pgDb -f $mig.FullName -v ON_ERROR_STOP=1 2>&1 | ForEach-Object { "$_" }
            if ($LASTEXITCODE -ne 0) {
                Write-Host " ERROR" -ForegroundColor Red
                Write-Host "  $output"
                $skipRest = Read-Host "  Continuar con las demas? (s/n)"
                if ($skipRest -ne "s") { exit 1 }
            } else {
                Write-Host " OK" -ForegroundColor Green
            }
        }

        $appMigs = Get-ChildItem "$InstallDir\migrations-app\*.sql" -ErrorAction SilentlyContinue | Sort-Object Name
        foreach ($mig in $appMigs) {
            Write-Host "  Ejecutando $($mig.Name)..." -NoNewline
            $output = & $PsqlPath -h $pgHost -p $pgPort -U $pgUser -d $pgDb -f $mig.FullName -v ON_ERROR_STOP=1 2>&1 | ForEach-Object { "$_" }
            if ($LASTEXITCODE -ne 0) {
                Write-Host " ERROR" -ForegroundColor Red
                Write-Host "  $output"
            } else {
                Write-Host " OK" -ForegroundColor Green
            }
        }

        $ErrorActionPreference = $prevEAP
        Remove-Item Env:\PGPASSWORD -ErrorAction SilentlyContinue
        Remove-Item Env:\PGOPTIONS -ErrorAction SilentlyContinue
        Write-Host "  Migraciones completadas." -ForegroundColor Green
    }
} else {
    Write-Host "`n[3/4] Migraciones omitidas (-SkipMigrations)" -ForegroundColor DarkYellow
}

# --- 4. Instalar servicio de Windows ---
if (-not $SkipService) {
    Write-Host "`n[4/4] Instalando servicio de Windows..." -ForegroundColor Yellow

    $exePath = "$InstallDir\lfsoft-sync.exe"
    $configPath = "$InstallDir\config\sync.yaml"
    $binPath = """$exePath"" --config ""$configPath"""

    Write-Host "  Creando servicio $ServiceName..."
    New-Service -Name $ServiceName `
        -BinaryPathName $binPath `
        -DisplayName $ServiceDisplayName `
        -StartupType Automatic `
        -Description "Servicio de sincronizacion bidireccional Access <-> PostgreSQL (LFSoft)" | Out-Null

    $svcCheck = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svcCheck) {
        Write-Host "  ERROR: No se pudo crear el servicio." -ForegroundColor Red
        Write-Host "  Intentando con sc.exe..."
        $scBinPath = "`"$exePath`" --config `"$configPath`""
        cmd /c "sc.exe create $ServiceName binPath= `"$scBinPath`" start= auto DisplayName= `"$ServiceDisplayName`""
        $svcCheck = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if (-not $svcCheck) {
            Write-Host "  ERROR: Tampoco funciono con sc.exe." -ForegroundColor Red
            Write-Host "  Ejecutar manualmente:"
            Write-Host "    sc.exe create $ServiceName binPath= `"$scBinPath`" start= auto"
            Write-Host ""
            Write-Host "  O ejecutar en modo consola:"
            Write-Host "    & `"$exePath`" --config `"$configPath`""
        }
    }

    if ($svcCheck) {
        sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/10000/restart/30000 | Out-Null

        Write-Host "  Servicio creado:" -ForegroundColor Green
        Write-Host "    Nombre:     $ServiceName"
        Write-Host "    Ejecutable: $exePath"
        Write-Host "    Config:     $configPath"
        Write-Host "    Inicio:     Automatico"
        Write-Host "    Recovery:   Reiniciar a los 5s, 10s, 30s"

        $startNow = Read-Host "`n  Iniciar el servicio ahora? (s/n)"
        if ($startNow -eq "s") {
            Start-Service -Name $ServiceName
            Start-Sleep -Seconds 3
            $svc = Get-Service -Name $ServiceName
            if ($svc.Status -eq "Running") {
                Write-Host "  Servicio iniciado correctamente." -ForegroundColor Green
            } else {
                Write-Host "  ADVERTENCIA: El servicio no esta corriendo (estado: $($svc.Status))" -ForegroundColor Red
                Write-Host "  Probar en modo consola para ver errores:"
                Write-Host "    & `"$exePath`" --config `"$configPath`""
            }
        }
    }
} else {
    Write-Host "`n[4/4] Servicio omitido (-SkipService)" -ForegroundColor DarkYellow
}

# --- Resumen final ---
Write-Host "`n=== Instalacion completada ===" -ForegroundColor Cyan
Write-Host ""
Write-Host "Estructura instalada:"
Get-ChildItem $InstallDir -Recurse -File | ForEach-Object {
    $rel = $_.FullName.Substring($InstallDir.Length + 1)
    Write-Host "  $rel"
}
Write-Host ""
Write-Host "--- Comandos utiles ---" -ForegroundColor Yellow
Write-Host "  Iniciar:    Start-Service $ServiceName"
Write-Host "  Detener:    Stop-Service $ServiceName"
Write-Host "  Estado:     Get-Service $ServiceName"
Write-Host "  Logs:       Get-Content $InstallDir\logs\sync.log -Tail 50 -Wait"
Write-Host "  Consola:    & `"$InstallDir\lfsoft-sync.exe`" --config `"$InstallDir\config\sync.yaml`""
Write-Host "  Init load:  & `"$InstallDir\lfsoft-sync.exe`" --config `"$InstallDir\config\sync.yaml`" --initial-load familias"
Write-Host "  Desinstalar: .\install-server.ps1 -Uninstall"
Write-Host ""
Write-Host "--- Carga inicial (ejecutar una vez por entidad) ---" -ForegroundColor Yellow
Write-Host "  Detener el servicio primero: Stop-Service $ServiceName"
$entityFiles2 = Get-ChildItem "$InstallDir\config\entities\*.yaml" -ErrorAction SilentlyContinue
foreach ($f in $entityFiles2) {
    $entityName = $f.BaseName
    $content = Get-Content $f.FullName -Raw -ErrorAction SilentlyContinue
    if ($content -match 'entity:\s*(\S+)') {
        $entityName = $Matches[1]
    }
    Write-Host "  & `"$InstallDir\lfsoft-sync.exe`" --config `"$InstallDir\config\sync.yaml`" --initial-load $entityName"
}
Write-Host "  Luego iniciar: Start-Service $ServiceName"
Write-Host ""
