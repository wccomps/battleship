package pods

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Guest is what configure-guest sets inside an environment host.
type Guest struct {
	OS       string `json:"os"` // ImageWindows or ImageLinux
	Hostname string `json:"hostname"`
	Domain   string `json:"domain"`
	IP       string `json:"ip"`
	Prefix   int    `json:"prefix"`
	Gateway  string `json:"gateway"`
	DNS      string `json:"dns"`
	// DNSOnly sets only the DNS server and suffix, on a host already set up.
	DNSOnly bool `json:"dns_only,omitempty"`
}

// RebootMarker is what a script prints when the change needs a restart
// (a Windows rename).
const RebootMarker = "battleship: reboot"

func NeedsReboot(out string) bool { return strings.Contains(out, RebootMarker) }

var macRE = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)

func isIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4() && a.String() == s
}

// Check validates every value the scripts embed. They run as root or SYSTEM,
// and the values come partly from VM notes, which users can edit.
func (g Guest) Check() error {
	if g.OS != ImageWindows && g.OS != ImageLinux {
		return fmt.Errorf("guest OS %q is neither %s nor %s", g.OS, ImageWindows, ImageLinux)
	}
	if err := CheckHostname(g.Hostname); err != nil {
		return err
	}
	if err := CheckDomain(g.Domain); err != nil {
		return err
	}
	if g.Prefix < 1 || g.Prefix > 32 {
		return fmt.Errorf("prefix length %d isn't 1-32", g.Prefix)
	}
	for _, a := range [][2]string{{"address", g.IP}, {"gateway", g.Gateway}, {"DNS server", g.DNS}} {
		if !isIPv4(a[1]) {
			return fmt.Errorf("%s %q isn't an IPv4 address", a[0], a[1])
		}
	}
	return nil
}

// GuestCommand is the agent command, and its stdin, that applies g to the
// NIC with mac.
func GuestCommand(g Guest, mac string) (command []string, input string, err error) {
	script, err := GuestScript(g, mac)
	if err != nil {
		return nil, "", err
	}
	if g.OS == ImageWindows {
		u := utf16.Encode([]rune(script))
		b := make([]byte, 2*len(u))
		for i, c := range u {
			b[2*i], b[2*i+1] = byte(c), byte(c>>8)
		}
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-EncodedCommand", base64.StdEncoding.EncodeToString(b)}, "", nil
	}
	return []string{"/bin/sh", "-c", linuxLocked}, script, nil
}

// quote single-quotes v for sh and PowerShell; Check has limited every
// value to names and addresses.
func quote(v string) string { return "'" + v + "'" }

// GuestScript is the script GuestCommand runs.
func GuestScript(g Guest, mac string) (string, error) {
	if err := g.Check(); err != nil {
		return "", err
	}
	if !macRE.MatchString(mac) {
		return "", fmt.Errorf("net0's MAC address %q isn't six hex pairs", mac)
	}
	if g.DNSOnly {
		if g.OS == ImageWindows {
			return fmt.Sprintf(windowsDNSScript, quote(strings.ToUpper(strings.ReplaceAll(mac, ":", "-"))), quote(g.DNS), quote(g.Domain)), nil
		}
		return fmt.Sprintf("MAC=%s\nDNS=%s\nDOMAIN=%s\n", quote(strings.ToLower(mac)), quote(g.DNS), quote(g.Domain)) + linuxFindIface + linuxDNSScript, nil
	}
	if g.OS == ImageWindows {
		return fmt.Sprintf(windowsScript, quote(strings.ToUpper(strings.ReplaceAll(mac, ":", "-"))), quote(g.IP), g.Prefix,
			quote(g.Gateway), quote(g.DNS), quote(g.Domain), quote(g.Hostname), RebootMarker), nil
	}
	return fmt.Sprintf("MAC=%s\nIP=%s\nPREFIX=%d\nGW=%s\nDNS=%s\nHOST=%s\nDOMAIN=%s\n",
		quote(strings.ToLower(mac)), quote(g.IP), g.Prefix, quote(g.Gateway), quote(g.DNS), quote(g.Hostname), quote(g.Domain)) + linuxFindIface + linuxScript, nil
}

// linuxFindIface sets $iface to the NIC with $MAC.
const linuxFindIface = `set -eu
iface=
for d in /sys/class/net/*; do
	if [ "$(cat "$d/address" 2>/dev/null)" = "$MAC" ]; then iface=${d##*/}; fi
done
if [ -z "$iface" ]; then echo "no network interface has MAC $MAC" >&2; exit 2; fi
`

// GuestLockWait is how long a setup script waits for another one still
// running in the guest (one a stopped or timed-out job left) before giving
// up with exit 75. It is under the executor's limit for a script, so the
// wait ends the script, not the executor.
const GuestLockWait = 4 * time.Minute

// linuxLocked is the agent's shell for Linux scripts (see lockedShell).
var linuxLocked = lockedShell("/run/battleship-guest.lock", GuestLockWait)

// lockedShell runs the script from stdin holding lock, waiting up to wait
// for it (a copy a stopped or timed-out job left running holds it), and
// exits 75 if it can't. The lock is taken on fd 9 with "timeout N flock 9",
// a form util-linux and busybox both have, and the script runs with fd 9
// closed, so a daemon it starts (an ifup hook, say) doesn't keep the lock:
// only this shell holds it, until the script ends. A guest without flock or
// timeout runs the script unlocked.
func lockedShell(lock string, wait time.Duration) string {
	return fmt.Sprintf(`if command -v flock >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
	exec 9>%s
	if ! timeout %s flock 9; then echo "another battleship setup script is still running in this guest" >&2; exit 75; fi
	/bin/sh -s 9>&-
	exit $?
fi
exec /bin/sh -s`, quote(lock), strconv.FormatFloat(wait.Seconds(), 'f', -1, 64))
}

// windowsLock holds a machine-wide mutex for the script the same way;
// Windows releases it when the process holding it exits.
var windowsLock = `$lock = New-Object System.Threading.Mutex($false, 'Global\battleship-guest')
try { $held = $lock.WaitOne([TimeSpan]::FromSeconds(` + strconv.Itoa(int(GuestLockWait.Seconds())) + `)) } catch [System.Threading.AbandonedMutexException] { $held = $true }
if (-not $held) { [Console]::Error.WriteLine('another battleship setup script is still running in this guest'); exit 75 }
`

// linuxScript sets a static address on the NIC with $MAC through whichever
// of netplan, NetworkManager or ifupdown the image uses, then the host name.
// cloud-init is told to leave both alone from now on.
const linuxScript = `if [ -d /etc/cloud/cloud.cfg.d ]; then
	printf 'network: {config: disabled}\npreserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-battleship.cfg
fi
if command -v netplan >/dev/null 2>&1 && [ -d /etc/netplan ]; then
	umask 077
	for f in /etc/netplan/*.yaml; do
		if [ -e "$f" ] && [ "$f" != /etc/netplan/99-battleship.yaml ]; then mv "$f" "$f.battleship-off"; fi
	done
	cat > /etc/netplan/99-battleship.yaml <<EOF
network:
  version: 2
  ethernets:
    $iface:
      dhcp4: false
      dhcp6: false
      addresses: [$IP/$PREFIX]
      routes: [{to: default, via: $GW}]
      nameservers: {addresses: [$DNS], search: [$DOMAIN]}
EOF
	netplan apply
elif command -v nmcli >/dev/null 2>&1 && nmcli -t general status >/dev/null 2>&1; then
	con=$(nmcli -t -f NAME,DEVICE connection show | awk -F: -v d="$iface" '$2 == d { print $1; exit }')
	if [ -z "$con" ]; then
		con=$iface
		nmcli connection add type ethernet ifname "$iface" con-name "$con" >/dev/null
	fi
	nmcli connection modify "$con" ipv4.method manual ipv4.addresses "$IP/$PREFIX" ipv4.gateway "$GW" \
		ipv4.dns "$DNS" ipv4.dns-search "$DOMAIN" ipv4.ignore-auto-dns yes ipv6.method disabled
	nmcli connection up "$con" >/dev/null
elif [ -f /etc/network/interfaces ]; then
	ifdown "$iface" 2>/dev/null || true
	mkdir -p /etc/network/interfaces.d /etc/network/battleship-off
	for f in /etc/network/interfaces.d/*; do
		[ -f "$f" ] && [ "$f" != /etc/network/interfaces.d/battleship ] || continue
		if grep -Eq "^[[:space:]]*(iface|auto|allow-hotplug)[[:space:]]+$iface([[:space:]]|$)" "$f"; then mv "$f" /etc/network/battleship-off/; fi
	done
	awk -v d="$iface" '
		($1 == "auto" || $1 == "allow-hotplug") && $2 == d && NF == 2 { next }
		$1 == "iface" { skip = ($2 == d) }
		$1 == "auto" || $1 == "allow-hotplug" || $1 == "source" || $1 == "mapping" { skip = 0 }
		!skip { print }' /etc/network/interfaces > /etc/network/interfaces.battleship
	mv /etc/network/interfaces.battleship /etc/network/interfaces
	grep -q '^source /etc/network/interfaces.d/\*' /etc/network/interfaces || echo 'source /etc/network/interfaces.d/*' >> /etc/network/interfaces
	cat > /etc/network/interfaces.d/battleship <<EOF
auto $iface
iface $iface inet static
	address $IP/$PREFIX
	gateway $GW
	dns-nameservers $DNS
	dns-search $DOMAIN
EOF
	[ -L /etc/resolv.conf ] || printf 'search %s\nnameserver %s\n' "$DOMAIN" "$DNS" > /etc/resolv.conf
	ifup "$iface"
else
	echo "found no netplan, NetworkManager or /etc/network/interfaces to configure" >&2
	exit 3
fi
if command -v hostnamectl >/dev/null 2>&1; then hostnamectl set-hostname "$HOST"; else echo "$HOST" > /etc/hostname; hostname "$HOST"; fi
sed -i '/^127\.0\.1\.1[[:space:]]/d' /etc/hosts
echo "127.0.1.1 $HOST.$DOMAIN $HOST" >> /etc/hosts
`

// windowsScript sets a static address on the adapter with the MAC, its DNS
// and suffix, and renames the computer; a rename prints RebootMarker.
var windowsScript = `$ErrorActionPreference = 'Stop'
` + windowsLock + `$mac = %s
$ip = %s
$prefix = %d
$gw = %s
$dns = %s
$domain = %s
$hostname = %s
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
	Write-Output '%s'
}
`

// linuxDNSScript changes only the DNS server and search domain of $iface,
// leaving its address, routes and other files alone.
const linuxDNSScript = `if command -v netplan >/dev/null 2>&1 && [ -d /etc/netplan ]; then
	umask 077
	f=/etc/netplan/99-battleship.yaml
	if [ -f "$f" ]; then
		awk -v l="      nameservers: {addresses: [$DNS], search: [$DOMAIN]}" '$1 == "nameservers:" { print l; next } { print }' "$f" > "$f.battleship"
		mv "$f.battleship" "$f"
	else
		cat > /etc/netplan/99-battleship-dns.yaml <<EOF
network:
  version: 2
  ethernets:
    $iface:
      nameservers: {addresses: [$DNS], search: [$DOMAIN]}
EOF
	fi
	netplan apply
elif command -v nmcli >/dev/null 2>&1 && nmcli -t general status >/dev/null 2>&1; then
	con=$(nmcli -t -f NAME,DEVICE connection show | awk -F: -v d="$iface" '$2 == d { print $1; exit }')
	if [ -z "$con" ]; then echo "no NetworkManager connection is on $iface" >&2; exit 3; fi
	nmcli connection modify "$con" ipv4.dns "$DNS" ipv4.dns-search "$DOMAIN" ipv4.ignore-auto-dns yes
	nmcli device reapply "$iface" >/dev/null
elif [ -f /etc/network/interfaces ]; then
	f=/etc/network/interfaces.d/battleship
	if [ -f "$f" ]; then
		awk -v n="$DNS" -v s="$DOMAIN" '$1 == "dns-nameservers" { print "\tdns-nameservers " n; next }
			$1 == "dns-search" { print "\tdns-search " s; next } { print }' "$f" > "$f.battleship"
		mv "$f.battleship" "$f"
	fi
	if [ -L /etc/resolv.conf ]; then
		if command -v resolvectl >/dev/null 2>&1; then
			resolvectl dns "$iface" "$DNS"
			resolvectl domain "$iface" "$DOMAIN"
		elif command -v resolvconf >/dev/null 2>&1; then
			printf 'nameserver %s\nsearch %s\n' "$DNS" "$DOMAIN" | resolvconf -a "$iface.inet"
		fi
	else
		printf 'search %s\nnameserver %s\n' "$DOMAIN" "$DNS" > /etc/resolv.conf
	fi
else
	echo "found no netplan, NetworkManager or /etc/network/interfaces to configure" >&2
	exit 3
fi
`

// windowsDNSScript sets only the DNS server and suffix of the adapter with
// the MAC.
var windowsDNSScript = `$ErrorActionPreference = 'Stop'
` + windowsLock + `$mac = %s
$dns = %s
$domain = %s
$nic = Get-NetAdapter | Where-Object { $_.MacAddress -eq $mac } | Select-Object -First 1
if (-not $nic) { [Console]::Error.WriteLine("no network adapter has MAC $mac"); exit 2 }
Set-DnsClientServerAddress -InterfaceIndex $nic.ifIndex -ServerAddresses $dns
Set-DnsClient -InterfaceIndex $nic.ifIndex -ConnectionSpecificSuffix $domain
`
