<#
.SYNOPSIS
    Firma digitalmente lfsoft-sync.exe (Authenticode, SHA256 + sello de tiempo).
.DESCRIPTION
    Usa Set-AuthenticodeSignature (no requiere el Windows SDK / signtool).
    El certificado se toma, en este orden:
      1. -PfxPath (archivo .pfx; pide la contrasena si no se pasa -PfxPassword)
      2. -Thumbprint (busca en CurrentUser\My y LocalMachine\My)
      3. Autodeteccion: el unico certificado de firma de codigo instalado
    Firmar CAMBIA el hash del archivo: el SHA256 que muestra al final es el que
    hay que pasar a IT si piden una excepcion por hash.
.EXAMPLE
    .\sign.ps1
    .\sign.ps1 -Thumbprint 1A2B3C...
    .\sign.ps1 -PfxPath C:\certs\firma.pfx
    .\sign.ps1 -FilePath .\lfsoft-sync.exe -TimestampServer ""   # sin sello de tiempo (sin internet)
#>
param(
    [string]$FilePath = (Join-Path $PSScriptRoot "lfsoft-sync.exe"),
    [string]$Thumbprint = "",
    [string]$PfxPath = "",
    [SecureString]$PfxPassword,
    [string]$TimestampServer = "http://timestamp.digicert.com"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $FilePath)) {
    Write-Host "ERROR: No se encuentra $FilePath" -ForegroundColor Red
    exit 1
}
$FilePath = (Resolve-Path $FilePath).Path
$codeSigningOid = "1.3.6.1.5.5.7.3.3"

# --- Seleccionar certificado ---
$cert = $null
if ($PfxPath) {
    if (-not $PfxPassword) { $PfxPassword = Read-Host "Contrasena del .pfx" -AsSecureString }
    $flags = [Security.Cryptography.X509Certificates.X509KeyStorageFlags]::Exportable
    $cert = New-Object Security.Cryptography.X509Certificates.X509Certificate2((Resolve-Path $PfxPath).Path, $PfxPassword, $flags)
} elseif ($Thumbprint) {
    $tp = ($Thumbprint -replace '\s', '').ToUpper()
    $cert = Get-ChildItem Cert:\CurrentUser\My, Cert:\LocalMachine\My -ErrorAction SilentlyContinue |
        Where-Object { $_.Thumbprint -eq $tp } | Select-Object -First 1
    if (-not $cert) {
        Write-Host "ERROR: No se encontro el certificado con thumbprint $tp" -ForegroundColor Red
        exit 1
    }
} else {
    $found = @(Get-ChildItem Cert:\CurrentUser\My, Cert:\LocalMachine\My -CodeSigningCert -ErrorAction SilentlyContinue |
        Where-Object { $_.NotAfter -gt (Get-Date) })
    if ($found.Count -eq 0) {
        Write-Host "ERROR: No hay certificados de firma de codigo vigentes instalados." -ForegroundColor Red
        Write-Host "Pasar -PfxPath o -Thumbprint, o instalar el certificado emitido por IT."
        exit 1
    }
    if ($found.Count -gt 1) {
        Write-Host "Hay varios certificados de firma de codigo. Elegir uno con -Thumbprint:" -ForegroundColor DarkYellow
        $found | ForEach-Object { Write-Host ("  {0}  {1}  (vence {2:yyyy-MM-dd})" -f $_.Thumbprint, $_.Subject, $_.NotAfter) }
        exit 1
    }
    $cert = $found[0]
}

# --- Validar certificado ---
if (-not $cert.HasPrivateKey) {
    Write-Host "ERROR: El certificado no tiene clave privada disponible." -ForegroundColor Red
    exit 1
}
$eku = $cert.Extensions | Where-Object { $_ -is [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension] }
if ($eku -and -not ($eku.EnhancedKeyUsages | Where-Object { $_.Value -eq $codeSigningOid })) {
    Write-Host "ERROR: El certificado no es de firma de codigo (falta EKU $codeSigningOid)." -ForegroundColor Red
    exit 1
}
if ($cert.NotAfter -lt (Get-Date)) {
    Write-Host "ERROR: El certificado vencio el $($cert.NotAfter)." -ForegroundColor Red
    exit 1
}

Write-Host "Archivo:     $FilePath"
Write-Host "Certificado: $($cert.Subject)"
Write-Host "Emisor:      $($cert.Issuer)"
Write-Host "Vence:       $($cert.NotAfter.ToString('yyyy-MM-dd'))"
Write-Host "Thumbprint:  $($cert.Thumbprint)"

# --- Firmar ---
$sigParams = @{
    FilePath      = $FilePath
    Certificate   = $cert
    HashAlgorithm = "SHA256"
    IncludeChain  = "NotRoot"
}
if ($TimestampServer) { $sigParams.TimestampServer = $TimestampServer }

$result = Set-AuthenticodeSignature @sigParams
if ($result.Status -ne "Valid" -and $result.Status -ne "UnknownError") {
    Write-Host "ERROR al firmar: $($result.Status) - $($result.StatusMessage)" -ForegroundColor Red
    exit 1
}

# --- Verificar ---
$check = Get-AuthenticodeSignature -FilePath $FilePath
Write-Host ""
switch ($check.Status) {
    "Valid" {
        Write-Host "Firma valida." -ForegroundColor Green
    }
    "UnknownError" {
        Write-Host "Firmado, pero esta PC no confia en la raiz del certificado (normal en autofirmados)." -ForegroundColor DarkYellow
        Write-Host "En el servidor hay que importar el certificado en 'Entidades de certificacion raiz de confianza' y 'Editores de confianza'."
    }
    default {
        Write-Host "ADVERTENCIA: estado de la firma: $($check.Status) - $($check.StatusMessage)" -ForegroundColor Red
        exit 1
    }
}
if ($check.TimeStamperCertificate) {
    Write-Host "Sello de tiempo: $($check.TimeStamperCertificate.Subject)"
} elseif ($TimestampServer) {
    Write-Host "ADVERTENCIA: sin sello de tiempo; la firma deja de ser valida cuando venza el certificado." -ForegroundColor DarkYellow
}

$hash = (Get-FileHash $FilePath -Algorithm SHA256).Hash
Write-Host ""
Write-Host "SHA256 (para excepcion por hash): $hash" -ForegroundColor Cyan
