# Personal Desktop first-logon bootstrap for the Windows guest.
#
# The unattend answer file runs this once from the sysprep CD-ROM, but every
# step is idempotent so an administrator can re-run it from
# C:\ProgramData\PersonalDesktop to repair a desktop in place.
#
# The operator substitutes @@PD_PYTHON_URL@@ and @@PD_PYTHON_SHA256@@ with
# spec.personalDesktop.windows.pythonInstaller when it renders the sysprep
# Secret. If the placeholders were left untouched (a manual run of the embedded
# script), PD_PYTHON_URL and PD_PYTHON_SHA256 are read from the environment
# instead. An installer is only downloaded when the golden image has no Python.

$ErrorActionPreference = 'Stop'

$AgentDirectory = Join-Path $env:ProgramData 'PersonalDesktop'
$AgentScript = Join-Path $AgentDirectory 'desktop_agent.py'
$TokenFile = Join-Path $AgentDirectory 'agent-token'
$LogFile = Join-Path $AgentDirectory 'setup.log'
$TaskName = 'PersonalDesktopAgent'
$FirewallRuleName = 'PersonalDesktopAgent'
$AgentPort = 9876

$PythonUrl = '@@PD_PYTHON_URL@@'
$PythonSha256 = '@@PD_PYTHON_SHA256@@'

function Write-Step {
    param([string] $Message)

    $line = '{0} {1}' -f (Get-Date -Format 'yyyy-MM-ddTHH:mm:ss'), $Message
    Write-Host $line
    try {
        Add-Content -Path $LogFile -Value $line -Encoding UTF8
    } catch {
        # The log is a convenience; a full or locked disk must not abort setup.
    }
}

$SourceDirectory = if ($PSScriptRoot) { $PSScriptRoot } `
    else { Split-Path -Parent $MyInvocation.MyCommand.Path }

function Initialize-RegistryKey {
    param([string] $Path)

    # New-Item -Force clears the values of a registry key that already exists,
    # which would wipe unrelated policy under these paths.
    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -Path $Path -Force | Out-Null
    }
}

function Copy-GuestFile {
    param([string] $SourceDirectory, [string] $Name, [string] $Destination)

    $source = Join-Path $SourceDirectory $Name
    if (-not (Test-Path -LiteralPath $source)) {
        if (Test-Path -LiteralPath $Destination) {
            Write-Step "$Name is already installed and the sysprep media is gone; keeping it"
            return
        }
        throw "$Name was not found next to setup.ps1 ($source) and is not installed yet"
    }
    if ((Resolve-Path -LiteralPath $source).Path -eq $Destination) { return }
    Copy-Item -LiteralPath $source -Destination $Destination -Force
    Write-Step "installed $Destination"
}

function Protect-GuestFile {
    param([string] $Path)

    # The guest token authenticates the Desktop Gateway, so only the desktop
    # user, SYSTEM and administrators may read it. SIDs are used because group
    # names are localized.
    & icacls.exe $Path /inheritance:r /grant:r "$($env:USERNAME):(R)" '*S-1-5-18:(F)' '*S-1-5-32-544:(F)' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls failed on $Path with exit code $LASTEXITCODE" }
}

function Get-PythonPath {
    foreach ($probe in @(@('py', '-3'), @('python'))) {
        $command = Get-Command $probe[0] -ErrorAction SilentlyContinue
        if (-not $command) { continue }
        $probeArguments = @()
        if ($probe.Count -gt 1) { $probeArguments += $probe[1] }
        $probeArguments += @('-c', 'import sys; sys.stdout.write(sys.executable)')
        $found = & $command.Source @probeArguments 2>$null
        if ($LASTEXITCODE -eq 0 -and $found -and (Test-Path -LiteralPath $found)) {
            return $found
        }
    }
    return $null
}

function Update-ProcessPath {
    # A fresh installation extends the machine PATH, which this already running
    # process would otherwise not see.
    $machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $user = [Environment]::GetEnvironmentVariable('Path', 'User')
    $env:Path = (@($machine, $user) | Where-Object { $_ }) -join ';'
}

function Install-Python {
    if ($PythonUrl -like '@@*@@') { $PythonUrl = $env:PD_PYTHON_URL }
    if ($PythonSha256 -like '@@*@@') { $PythonSha256 = $env:PD_PYTHON_SHA256 }
    if (-not $PythonUrl -or -not $PythonSha256) {
        throw 'Python 3 is missing and no verified installer was provided (PD_PYTHON_URL / PD_PYTHON_SHA256)'
    }

    $installer = Join-Path $env:TEMP 'personal-desktop-python.exe'
    Write-Step "downloading the Python installer from $PythonUrl"
    # Windows Server defaults can still negotiate TLS 1.0, which python.org
    # refuses, and the progress bar makes Invoke-WebRequest an order of
    # magnitude slower.
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $ProgressPreference = 'SilentlyContinue'
    Invoke-WebRequest -Uri $PythonUrl -OutFile $installer -UseBasicParsing

    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $installer).Hash
    if ($actual -ne $PythonSha256.ToUpperInvariant()) {
        Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue
        throw "the Python installer digest is $actual, expected $PythonSha256"
    }

    Write-Step 'installing Python 3 for all users'
    $process = Start-Process -FilePath $installer -Wait -PassThru -ArgumentList @(
        '/quiet', 'InstallAllUsers=1', 'PrependPath=1', 'Include_test=0'
    )
    Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue
    if ($process.ExitCode -ne 0) {
        throw "the Python installer exited with $($process.ExitCode)"
    }
    Update-ProcessPath
}

# --- install the agent -------------------------------------------------

New-Item -ItemType Directory -Path $AgentDirectory -Force | Out-Null
Write-Step "Personal Desktop setup starting for $env:USERNAME"

Copy-GuestFile -SourceDirectory $SourceDirectory -Name 'desktop_agent.py' -Destination $AgentScript
Copy-GuestFile -SourceDirectory $SourceDirectory -Name 'agent-token' -Destination $TokenFile
Protect-GuestFile -Path $TokenFile
Protect-GuestFile -Path $AgentScript

# --- Python ------------------------------------------------------------

$python = Get-PythonPath
if (-not $python) {
    Install-Python
    $python = Get-PythonPath
}
if (-not $python) { throw 'Python 3 is still not available after installation' }
Write-Step "using the Python interpreter at $python"

# --- keep the session awake, unlocked and reachable --------------------

# An idle desktop that sleeps, blanks or locks itself returns black frames and
# rejects typed input, and nobody is at the console to wake it.
foreach ($setting in @('standby-timeout-ac', 'standby-timeout-dc',
                       'monitor-timeout-ac', 'monitor-timeout-dc',
                       'hibernate-timeout-ac', 'hibernate-timeout-dc')) {
    & powercfg.exe /change $setting 0 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "powercfg /change $setting failed with $LASTEXITCODE" }
}
Write-Step 'disabled sleep, hibernation and monitor timeouts'

Initialize-RegistryKey -Path 'HKCU:\Control Panel\Desktop'
Set-ItemProperty -Path 'HKCU:\Control Panel\Desktop' -Name 'ScreenSaveActive' -Value '0'
Set-ItemProperty -Path 'HKCU:\Control Panel\Desktop' -Name 'ScreenSaveTimeOut' -Value '0'
Set-ItemProperty -Path 'HKCU:\Control Panel\Desktop' -Name 'ScreenSaverIsSecure' -Value '0'

$personalization = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Personalization'
Initialize-RegistryKey -Path $personalization
Set-ItemProperty -Path $personalization -Name 'NoLockScreen' -Value 1 -Type DWord

$systemPolicies = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System'
Initialize-RegistryKey -Path $systemPolicies
Set-ItemProperty -Path $systemPolicies -Name 'InactivityTimeoutSecs' -Value 0 -Type DWord

$userPolicies = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Policies\System'
Initialize-RegistryKey -Path $userPolicies
Set-ItemProperty -Path $userPolicies -Name 'DisableLockWorkstation' -Value 1 -Type DWord
Write-Step 'disabled the screen saver and the lock screen'

if (-not (Get-NetFirewallRule -Name $FirewallRuleName -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name $FirewallRuleName `
        -DisplayName 'Personal Desktop guest agent' `
        -Description 'Typed computer-use actions from the Desktop Gateway' `
        -Direction Inbound -Action Allow -Protocol TCP -LocalPort $AgentPort `
        -Profile Any | Out-Null
    Write-Step "opened inbound TCP $AgentPort"
} else {
    Write-Step "inbound TCP $AgentPort is already open"
}

# --- run the agent in the interactive session --------------------------

# /it runs the task with the interactive token, which is what makes the agent
# able to drive the visible desktop; python.exe rather than pythonw.exe keeps
# the agent's stderr visible in the session for troubleshooting.
$taskUser = "$env:USERDOMAIN\$env:USERNAME"
$taskCommand = '\"{0}\" \"{1}\"' -f $python, $AgentScript
& schtasks.exe /create /sc onlogon /it /ru $taskUser /tn $TaskName /tr $taskCommand /f | Out-Null
if ($LASTEXITCODE -ne 0) { throw "schtasks /create failed with exit code $LASTEXITCODE" }
Write-Step "registered the $TaskName logon task"

& schtasks.exe /run /tn $TaskName | Out-Null
if ($LASTEXITCODE -ne 0) {
    # A task that is already running is not a setup failure, and the logon
    # trigger starts it after the next reboot in any case.
    Write-Step "schtasks /run returned $LASTEXITCODE; the task starts at the next logon"
} else {
    Write-Step "started $TaskName"
}

Write-Step 'Personal Desktop setup finished'
