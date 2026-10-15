MAC='bc:24:11:00:00:01'
DNS='192.168.100.2'
DOMAIN='kilo.alpha'
set -eu
iface=
for d in /sys/class/net/*; do
	if [ "$(cat "$d/address" 2>/dev/null)" = "$MAC" ]; then iface=${d##*/}; fi
done
if [ -z "$iface" ]; then echo "no network interface has MAC $MAC" >&2; exit 2; fi
if command -v netplan >/dev/null 2>&1 && [ -d /etc/netplan ]; then
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
