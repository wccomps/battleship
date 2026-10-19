MAC='bc:24:11:00:00:01'
IP='192.168.100.10'
PREFIX=24
GW='192.168.100.2'
DNS='192.168.100.2'
HOST='teak'
DOMAIN='kilo.alpha'
CA='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILM+rvN+ot98qgEN796jTiQfZfG1KaT0PtFDJ/XFSqti'
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
		$1 == "auto" || $1 == "allow-hotplug" || $1 == "source" || $1 == "source-directory" || $1 == "mapping" { skip = 0 }
		!skip { print }' /etc/network/interfaces > /etc/network/interfaces.battleship
	mv /etc/network/interfaces.battleship /etc/network/interfaces
	# ifupdown-ng (Alpine) reads source-directory, but not a glob in source.
	grep -Eq '^(source /etc/network/interfaces\.d/\*|source-directory /etc/network/interfaces\.d/?)[[:space:]]*$' /etc/network/interfaces ||
		echo 'source-directory /etc/network/interfaces.d' >> /etc/network/interfaces
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
if [ -n "$CA" ]; then
	mkdir -p /etc/ssh/auth_principals /root/.ssh
	chmod 700 /root/.ssh
	printf '%s\n' "$CA" > /etc/ssh/vault-ca.pub
	chmod 644 /etc/ssh/vault-ca.pub
	touch /etc/ssh/auth_principals/root
	for p in core-infrastructure competition-infrastructure root; do
		grep -qxF "$p" /etc/ssh/auth_principals/root || printf '%s\n' "$p" >> /etc/ssh/auth_principals/root
	done
	chmod 644 /etc/ssh/auth_principals/root
	ak="cert-authority,principals=\"core-infrastructure,competition-infrastructure,root\" $CA"
	touch /root/.ssh/authorized_keys
	grep -qxF "$ak" /root/.ssh/authorized_keys || printf '%s\n' "$ak" >> /root/.ssh/authorized_keys
	chmod 600 /root/.ssh/authorized_keys
	if [ -f /etc/ssh/sshd_config ]; then
		awk '
			tolower($1) == "trustedusercakeys" || tolower($1) == "authorizedprincipalsfile" { next }
			tolower($1) == "match" && !done {
				print "TrustedUserCAKeys /etc/ssh/vault-ca.pub"
				print "AuthorizedPrincipalsFile /etc/ssh/auth_principals/%u"
				done = 1
			}
			{ print }
			END {
				if (!done) {
					print "TrustedUserCAKeys /etc/ssh/vault-ca.pub"
					print "AuthorizedPrincipalsFile /etc/ssh/auth_principals/%u"
				}
			}' /etc/ssh/sshd_config > /etc/ssh/sshd_config.battleship
		chmod 600 /etc/ssh/sshd_config.battleship
		mv /etc/ssh/sshd_config.battleship /etc/ssh/sshd_config
		systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null || systemctl restart sshd 2>/dev/null || systemctl restart ssh 2>/dev/null || service sshd reload 2>/dev/null || service ssh reload 2>/dev/null || true
	fi
fi
