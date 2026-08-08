# Lykuro Native LLM Platform - Private Gateway Windows 常駐登録
# 管理者 PowerShell で実行: powershell -ExecutionPolicy Bypass -File deploy\native\install-task.ps1
# Go の console バイナリは SCM ハンドシェイクを実装していないため、Windows
# サービス(sc.exe)ではなくスケジュールタスク(起動時実行)として登録する。
$ErrorActionPreference = "Stop"

$installDir = "C:\Program Files\Lykuro\PrivateGateway"
$configDir  = "C:\ProgramData\Lykuro\Gateway"

# install token は初回登録時のみ参照される(serve は env LYKURO_INSTALL_TOKEN_FILE を読む)
[Environment]::SetEnvironmentVariable("LYKURO_INSTALL_TOKEN_FILE", "$configDir\install-token.txt", "Machine")

$action = New-ScheduledTaskAction -Execute "$installDir\private-gateway.exe" `
  -Argument "serve -config ""$configDir\gateway.yaml"""
$trigger  = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
  -ExecutionTimeLimit (New-TimeSpan -Days 0)
Register-ScheduledTask -TaskName "LykuroPrivateGateway" `
  -Action $action -Trigger $trigger -Settings $settings `
  -User "NT AUTHORITY\SYSTEM" -RunLevel Highest -Force

Start-ScheduledTask -TaskName "LykuroPrivateGateway"
Write-Host "registered and started: LykuroPrivateGateway"
