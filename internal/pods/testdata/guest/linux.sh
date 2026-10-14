MAC='bc:24:11:00:00:01'
IP='192.168.100.10'
PREFIX=24
GW='192.168.100.2'
DNS='192.168.100.2'
HOST='teak'
DOMAIN='kilo.alpha'
set -eu
iface=
for d in /sys/class/net/*; do
	if [ "$(cat "$d/address" 2>/dev/null)" = "$MAC" ]; then iface=${d##*/}; fi
done
if [ -z "$iface" ]; then echo "no network interface has MAC $MAC" >&2; exit 2; fi
if [ -d /etc/cloud/cloud.cfg.d ]; then
	printf 'network: {config: disabled}\npreserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-battleship.cfg
fi
if command -v netplan >/dev/null 2>&1 && [ -d /etc/netplan ]; then
	umask 077
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
	awk -v d="$iface" '
		($1 == "auto" || $1 == "allow-hotplug") && $2 == d && NF == 2 { next }
		$1 == "iface" { skip = ($2 == d) }
		$1 == "auto" || $1 == "allow-hotplug" || $1 == "source" || $1 == "mapping" { skip = 0 }
		!skip { print }' /etc/network/interfaces > /etc/network/interfaces.battleship
	mv /etc/network/interfaces.battleship /etc/network/interfaces
	grep -q '^source /etc/network/interfaces.d/\*' /etc/network/interfaces || echo 'source /etc/network/interfaces.d/*' >> /etc/network/interfaces
	mkdir -p /etc/network/interfaces.d
	cat > /etc/network/interfaces.d/battleship <<EOF
auto $iface
iface $iface inet static
	address $IP/$PREFIX
	gateway $GW
EOF
	printf 'search %s\nnameserver %s\n' "$DOMAIN" "$DNS" > /etc/resolv.conf
	ip addr flush dev "$iface"
	ifup "$iface" 2>/dev/null || ifup --force "$iface"
else
	echo "found no netplan, NetworkManager or /etc/network/interfaces to configure" >&2
	exit 3
fi
if command -v hostnamectl >/dev/null 2>&1; then hostnamectl set-hostname "$HOST"; else echo "$HOST" > /etc/hostname; hostname "$HOST"; fi
sed -i '/^127\.0\.1\.1[[:space:]]/d' /etc/hosts
echo "127.0.1.1 $HOST.$DOMAIN $HOST" >> /etc/hosts
