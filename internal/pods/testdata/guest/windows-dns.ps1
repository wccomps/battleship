$ErrorActionPreference = 'Stop'
$lock = New-Object System.Threading.Mutex($false, 'Global\battleship-guest')
try { $held = $lock.WaitOne([TimeSpan]::FromMinutes(4)) } catch [System.Threading.AbandonedMutexException] { $held = $true }
if (-not $held) { [Console]::Error.WriteLine('another battleship setup script is still running in this guest'); exit 75 }
$mac = 'BC-24-11-00-00-01'
$dns = '192.168.100.2'
$domain = 'kilo.alpha'
$nic = Get-NetAdapter | Where-Object { $_.MacAddress -eq $mac } | Select-Object -First 1
if (-not $nic) { [Console]::Error.WriteLine("no network adapter has MAC $mac"); exit 2 }
Set-DnsClientServerAddress -InterfaceIndex $nic.ifIndex -ServerAddresses $dns
Set-DnsClient -InterfaceIndex $nic.ifIndex -ConnectionSpecificSuffix $domain
