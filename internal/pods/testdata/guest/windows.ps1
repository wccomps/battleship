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
$ca = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILM+rvN+ot98qgEN796jTiQfZfG1KaT0PtFDJ/XFSqti'
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
if ($ca -and (Get-Service sshd -ErrorAction SilentlyContinue)) {
	$sshDir = "$env:ProgramData\ssh"
	$prDir = "$sshDir\auth_principals"
	New-Item -ItemType Directory -Path $prDir -Force | Out-Null
	Set-Content -LiteralPath "$sshDir\vault-ca.pub" -Value $ca -Encoding ascii
	$principals = @('core-infrastructure', 'competition-infrastructure', 'administrator', 'administrators')
	foreach ($u in 'administrators', 'administrator', 'root') {
		Set-Content -LiteralPath "$prDir\$u" -Value $principals -Encoding ascii
	}
	$akFile = "$sshDir\administrators_authorized_keys"
	$akLine = 'cert-authority,principals="core-infrastructure,competition-infrastructure,administrator,administrators" ' + $ca
	$existing = @()
	if (Test-Path -LiteralPath $akFile) { $existing = @(Get-Content -LiteralPath $akFile) }
	if ($existing -notcontains $akLine) {
		Set-Content -LiteralPath $akFile -Value ($existing + $akLine) -Encoding ascii
	}
	foreach ($p in "$sshDir\vault-ca.pub", $prDir, "$prDir\administrators", "$prDir\administrator", "$prDir\root", $akFile) {
		icacls.exe $p /inheritance:r /grant '*S-1-5-18:(F)' /grant '*S-1-5-32-544:(F)' | Out-Null
	}
	$cfgFile = "$sshDir\sshd_config"
	if (Test-Path -LiteralPath $cfgFile) {
		$caLines = @('TrustedUserCAKeys __PROGRAMDATA__/ssh/vault-ca.pub', 'AuthorizedPrincipalsFile __PROGRAMDATA__/ssh/auth_principals/administrators')
		$out = New-Object System.Collections.Generic.List[string]
		$inserted = $false
		foreach ($line in Get-Content -LiteralPath $cfgFile) {
			if ($line -match '^\s*(TrustedUserCAKeys|AuthorizedPrincipalsFile)\b') { continue }
			if (-not $inserted -and $line -match '^\s*Match\b') {
				$out.AddRange([string[]]$caLines)
				$inserted = $true
			}
			$out.Add($line)
		}
		if (-not $inserted) { $out.AddRange([string[]]$caLines) }
		Set-Content -LiteralPath $cfgFile -Value $out -Encoding ascii
	}
	Set-Service -Name sshd -StartupType Automatic
	Restart-Service -Name sshd -Force
}
if ($env:COMPUTERNAME -ne $hostname.ToUpper()) {
	Rename-Computer -NewName $hostname -Force | Out-Null
	Write-Output 'battleship: reboot'
}
