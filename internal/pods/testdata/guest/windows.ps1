$ErrorActionPreference = 'Stop'
$lock = New-Object System.Threading.Mutex($false, 'Global\battleship-guest')
try { $held = $lock.WaitOne([TimeSpan]::FromSeconds(240)) } catch [System.Threading.AbandonedMutexException] { $held = $true }
if (-not $held) { [Console]::Error.WriteLine('another battleship setup script is still running in this guest'); exit 75 }
$mac = 'BC-24-11-00-00-01'
$ip = '192.168.100.10'
$prefix = 24
$gw = '192.168.100.2'
$dns = '192.168.100.2'
$domain = 'kilo.alpha'
$hostname = 'teak'
$nic = Get-NetAdapter | Where-Object { $_.MacAddress -eq $mac } | Select-Object -First 1
if (-not $nic) { [Console]::Error.WriteLine("no network adapter has MAC $mac"); exit 2 }
$i = $nic.ifIndex
Set-NetIPInterface -InterfaceIndex $i -Dhcp Disabled
foreach ($store in 'ActiveStore', 'PersistentStore') {
	Get-NetIPAddress -InterfaceIndex $i -AddressFamily IPv4 -PolicyStore $store -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -ne $ip -or $_.PrefixLength -ne $prefix } | Remove-NetIPAddress -PolicyStore $store -Confirm:$false -ErrorAction SilentlyContinue
	Get-NetRoute -InterfaceIndex $i -DestinationPrefix '0.0.0.0/0' -PolicyStore $store -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -ne $gw } | Remove-NetRoute -PolicyStore $store -Confirm:$false -ErrorAction SilentlyContinue
}
if (-not (Get-NetIPAddress -InterfaceIndex $i -IPAddress $ip -ErrorAction SilentlyContinue)) {
	New-NetIPAddress -InterfaceIndex $i -IPAddress $ip -PrefixLength $prefix | Out-Null
}
if (-not (Get-NetRoute -InterfaceIndex $i -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -eq $gw })) {
	New-NetRoute -InterfaceIndex $i -DestinationPrefix '0.0.0.0/0' -NextHop $gw | Out-Null
}
Set-DnsClientServerAddress -InterfaceIndex $i -ServerAddresses $dns
Set-DnsClient -InterfaceIndex $i -ConnectionSpecificSuffix $domain
if ($env:COMPUTERNAME -ne $hostname.ToUpper()) {
	Rename-Computer -NewName $hostname -Force | Out-Null
	Write-Output 'battleship: reboot'
}
