# Lykuro Native LLM Platform - Windows インストーラ
#
# 管理者 PowerShell で実行:
#   irm https://raw.githubusercontent.com/lykuroai/Native-LLM-Platform/main/deploy/native/install.ps1 | iex
# バージョン指定:
#   & ([scriptblock]::Create((irm .../install.ps1))) -Version v0.1.0
#
# GitHub Releases からバイナリを取得し、checksums.txt と SHA-256 を突合して
# から配置する(検証済みファイルのみ Unblock-File する。ブラウザ経由の
# ダウンロードで出る SmartScreen 警告はこの経路では発生しない)。
param(
  [string]$Version = "",
  [string]$InstallDir = "C:\Program Files\Lykuro\PrivateGateway",
  [string]$ConfigDir  = "C:\ProgramData\Lykuro\Gateway"
)
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = "lykuroai/Native-LLM-Platform"
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "管理者 PowerShell で実行してください"
}

if ($Version -eq "") {
  $Version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
}
$exeName = "private-gateway_${Version}_windows_amd64.exe"
$base    = "https://github.com/$repo/releases/download/$Version"
$tmp     = Join-Path $env:TEMP "lykuro-pgw-$Version"
New-Item -ItemType Directory -Force $tmp | Out-Null

Write-Host "downloading $exeName ($Version)..."
Invoke-WebRequest "$base/$exeName"        -OutFile (Join-Path $tmp $exeName)
Invoke-WebRequest "$base/checksums.txt"   -OutFile (Join-Path $tmp "checksums.txt")

# SHA-256 検証(不一致は即中止)
$expected = (Select-String -Path (Join-Path $tmp "checksums.txt") -Pattern ([regex]::Escape($exeName))).Line.Split(' ')[0]
if (-not $expected) { throw "checksums.txt に $exeName のエントリがありません" }
$actual = (Get-FileHash (Join-Path $tmp $exeName) -Algorithm SHA256).Hash.ToLower()
if ($actual -ne $expected.ToLower()) {
  throw "SHA-256 不一致: expected=$expected actual=$actual — ダウンロードが破損または改ざんされています"
}
Write-Host "checksum OK: $actual"
Unblock-File (Join-Path $tmp $exeName)

# 配置
New-Item -ItemType Directory -Force $InstallDir, $ConfigDir | Out-Null
Copy-Item (Join-Path $tmp $exeName) (Join-Path $InstallDir "private-gateway.exe") -Force
Write-Host "installed: $InstallDir\private-gateway.exe"

# 設定テンプレート(既存の gateway.yaml は上書きしない)
$cfg = Join-Path $ConfigDir "gateway.yaml"
if (-not (Test-Path $cfg)) {
  Invoke-WebRequest "https://raw.githubusercontent.com/$repo/main/config/gateway.example.yaml" -OutFile $cfg
  Write-Host "config template: $cfg (要編集)"
}

# install token は初回登録時のみ参照される(serve は env LYKURO_INSTALL_TOKEN_FILE を読む)
[Environment]::SetEnvironmentVariable("LYKURO_INSTALL_TOKEN_FILE", "$ConfigDir\install-token.txt", "Machine")

# 常駐登録(起動時実行のスケジュールタスク。Go の console バイナリは SCM
# 非対応のため Windows サービスではなくタスク方式)
$action = New-ScheduledTaskAction -Execute "$InstallDir\private-gateway.exe" `
  -Argument "serve -config ""$cfg"""
$trigger  = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
  -ExecutionTimeLimit (New-TimeSpan -Days 0)
Register-ScheduledTask -TaskName "LykuroPrivateGateway" `
  -Action $action -Trigger $trigger -Settings $settings `
  -User "NT AUTHORITY\SYSTEM" -RunLevel Highest -Force | Out-Null
Write-Host "scheduled task registered: LykuroPrivateGateway"

& (Join-Path $InstallDir "private-gateway.exe") version

Write-Host ""
Write-Host "次の手順:"
Write-Host "  1. $cfg を編集(Gateway ID・models・virtual key)"
Write-Host "  2. 管理コンソールで発行した初回登録トークンを $ConfigDir\install-token.txt へ保存"
Write-Host "  3. 検証: & '$InstallDir\private-gateway.exe' config validate -config '$cfg'"
Write-Host "  4. 起動: Start-ScheduledTask -TaskName LykuroPrivateGateway"
