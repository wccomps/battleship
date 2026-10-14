$ErrorActionPreference = 'Stop'
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
Get-NetIPAddress -InterfaceIndex $i -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -ne $ip } | Remove-NetIPAddress -Confirm:$false
Get-NetRoute -InterfaceIndex $i -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Remove-NetRoute -Confirm:$false
if (Get-NetIPAddress -InterfaceIndex $i -IPAddress $ip -ErrorAction SilentlyContinue) {
	New-NetRoute -InterfaceIndex $i -DestinationPrefix '0.0.0.0/0' -NextHop $gw | Out-Null
} else {
	New-NetIPAddress -InterfaceIndex $i -IPAddress $ip -PrefixLength $prefix -DefaultGateway $gw | Out-Null
}
Set-DnsClientServerAddress -InterfaceIndex $i -ServerAddresses $dns
Set-DnsClient -InterfaceIndex $i -ConnectionSpecificSuffix $domain
if ($env:COMPUTERNAME -ne $hostname.ToUpper()) {
	Rename-Computer -NewName $hostname -Force | Out-Null
	Write-Output 'battleship: reboot'
}
