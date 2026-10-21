package pods

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// An environment's VMs have the addresses battleship assigned them (EnvHost
// Addr), and the ones their guest agents report (LiveAddrs).

// guestAddr is the address the agent reports on the NIC with MAC mac, with
// its prefix length as GuestNIC.IPs has it: its first IPv4 address, else
// its first IPv6 one, skipping loopback, link-local, multicast and
// unspecified addresses. Its Addr is invalid if there is none.
func guestAddr(nics []proxmox.GuestNIC, mac string) netip.Prefix {
	if mac == "" {
		return netip.Prefix{}
	}
	var v6 netip.Prefix
	for _, n := range nics {
		if !strings.EqualFold(n.MAC, mac) {
			continue
		}
		for _, ip := range n.IPs {
			switch a := ip.Addr(); {
			case !isHostAddr(a):
			case a.Is4():
				return ip
			case !v6.Addr().IsValid():
				v6 = ip
			}
		}
	}
	return v6
}

// isHostAddr is whether a can be a host's own address.
func isHostAddr(a netip.Addr) bool {
	return !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast() && !a.IsMulticast() && !a.IsUnspecified() &&
		a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// externalAddr is the address of LAN host octet from outside, given the
// router's WAN address and prefix. It assumes the router image's one-to-one
// NAT of LAN .N to WAN /24 .N, which battleship neither configures nor
// checks, so there is none unless wan is an IPv4 /24 and octet 1 to 254.
func externalAddr(wan netip.Prefix, octet int) string {
	l, ok := config.LANOf(wan)
	if !ok || octet < 1 || octet > 254 {
		return ""
	}
	return l.Host(octet).String()
}

// LiveAddrs are a VM's addresses and SSH state as its guest agent reports them.
type LiveAddrs struct {
	// Live: its agent answered; LAN, Differs, WAN and SSHWarning are set only then.
	Live bool
	LAN  string // on its LAN NIC
	// Differs says LAN isn't its expected address: the gateway for a
	// router, else its assigned address (EnvHost.Addr) if it has one.
	Differs bool
	WAN     string // the router's, on its WAN NIC
	// External is a host's address from outside, through the router's NAT
	// (externalAddr): by its live LAN octet, else its assigned one.
	External string
	// SSHWarning is why SSH or the ops CA key isn't ready on the host, or
	// "" when it is (or couldn't be checked).
	SSHWarning string
}

// agentAddrs are what a host's agent reported on its LAN and WAN NICs and SSH check.
type agentAddrs struct {
	lan, wan   netip.Prefix
	sshWarning string
}

// addrs are e's hosts' addresses, by VMID, from what their agents reported
// (readAgents); a host missing from read isn't live.
func (e Environment) addrs(read map[int]agentAddrs) map[int]LiveAddrs {
	out := map[int]LiveAddrs{}
	var wan netip.Prefix
	for _, h := range e.Hosts {
		got, live := read[h.VMID]
		a := LiveAddrs{Live: live}
		if live {
			if got.lan.Addr().IsValid() {
				a.LAN = got.lan.Addr().String()
			}
			want := h.Addr
			if h.Router {
				want = e.Gateway()
			}
			a.Differs = want.IsValid() && got.lan.Addr() != want
			if h.Router && got.wan.Addr().IsValid() {
				a.WAN = got.wan.Addr().String()
				if !wan.Addr().IsValid() {
					wan = got.wan
				}
			}
			a.SSHWarning = got.sshWarning
		}
		out[h.VMID] = a
	}
	for _, h := range e.Hosts {
		a := out[h.VMID]
		if h.Router || !wan.Addr().IsValid() {
			continue
		}
		octet, _ := e.LAN.Octet(h.Addr)
		if lanAddr := read[h.VMID].lan.Addr(); lanAddr.Is4() {
			octet, _ = e.LAN.Octet(lanAddr)
		}
		a.External = externalAddr(wan, octet)
		out[h.VMID] = a
	}
	return out
}

// asksAgent is whether h's agent may report its addresses: it is running,
// with its agent enabled and a NIC to look on.
func (h EnvHost) asksAgent() bool {
	return h.Status == "running" && h.Agent && (h.LANMAC != "" || h.WANMAC != "")
}

// LiveAddrs are envs' hosts' addresses and SSH state by VMID, read through
// their guest agents, reads at a time (at least one), each host bounded by
// timeout and all by ctx. A VM whose agent isn't asked (asksAgent), or whose
// interface read fails or isn't made before ctx ends, isn't live; the SSH
// check (AgentRead) is best-effort once interfaces are read.
func (p Planner) LiveAddrs(ctx context.Context, envs []Environment, reads int, timeout time.Duration) map[int]LiveAddrs {
	read := p.readAgents(ctx, envs, reads, timeout)
	out := map[int]LiveAddrs{}
	for _, e := range envs {
		maps.Copy(out, e.addrs(read))
	}
	return out
}

// readAgents reads the addresses and SSH state of envs' hosts whose agents
// may report them (asksAgent), as LiveAddrs says. Each answer is cut to its
// addresses and warning as it comes, so no more than reads answers are held
// at once.
func (p Planner) readAgents(ctx context.Context, envs []Environment, reads int, timeout time.Duration) map[int]agentAddrs {
	var mu sync.Mutex
	out := map[int]agentAddrs{}
	var g errgroup.Group
	g.SetLimit(max(reads, 1))
	for _, e := range envs {
		for _, h := range e.Hosts {
			if !h.asksAgent() || ctx.Err() != nil {
				continue
			}
			g.Go(func() error {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				nics, err := p.API.AgentInterfaces(cctx, h.Node, h.VMID)
				if err != nil {
					return nil
				}
				sshWarn := p.checkHostSSH(cctx, h, nics)
				mu.Lock()
				out[h.VMID] = agentAddrs{lan: guestAddr(nics, h.LANMAC), wan: guestAddr(nics, h.WANMAC), sshWarning: sshWarn}
				mu.Unlock()
				return nil
			})
		}
	}
	_ = g.Wait() // no read fails the group
	return out
}

const (
	maxSSHCheckOutput = 64 << 10
	sshPollInterval   = 50 * time.Millisecond
)

var posixSSHCheck = []string{"sh", "-c",
	`if systemctl is-active --quiet sshd 2>/dev/null || systemctl is-active --quiet ssh 2>/dev/null || systemctl is-active --quiet ssh.socket 2>/dev/null || pgrep -x sshd >/dev/null 2>&1 || ps ax 2>/dev/null | grep -v grep | grep -q "/sshd"; then echo sshd=running; else echo sshd=stopped; fi
echo ===SSHD===
cfg=$({ sshd -T || /usr/sbin/sshd -T || /usr/local/sbin/sshd -T; } 2>/dev/null || grep -Ehv "^[[:space:]]*(#|$)" /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf /usr/local/etc/ssh/sshd_config 2>/dev/null || true)
printf "%s\n" "$cfg" | grep -E -i "^[[:space:]]*(permitrootlogin|pubkeyauthentication|trustedusercakeys|authorizedprincipalsfile|authorizedkeysfile)[[:space:]]" || true
ca_file=$(printf "%s\n" "$cfg" | awk 'tolower($1) == "trustedusercakeys" && tolower($2) != "none" { print $2; exit }')
pr_file=$(printf "%s\n" "$cfg" | awk 'tolower($1) == "authorizedprincipalsfile" && tolower($2) != "none" { gsub(/%u/, "root", $2); gsub(/%h/, "/root", $2); print $2; exit }')
echo ===CA===
[ -n "$ca_file" ] && cat "$ca_file" 2>/dev/null || true
echo ===PRINCIPALS===
[ -n "$pr_file" ] && cat "$pr_file" 2>/dev/null || true
echo ===AUTHKEYS===
cat /root/.ssh/authorized_keys /root/.ssh/authorized_keys2 2>/dev/null || true`}

var windowsSSHCheck = []string{"cmd.exe", "/c",
	`sc query sshd | findstr /I "RUNNING" >nul && (echo sshd=running) || (echo sshd=stopped)` +
		` & echo ===SSHD===` +
		` & ("%SystemRoot%\System32\OpenSSH\sshd.exe" -T 2>nul || type "%ProgramData%\ssh\sshd_config" 2>nul) | findstr /I /R "^[ \t]*permitrootlogin ^[ \t]*pubkeyauthentication ^[ \t]*trustedusercakeys ^[ \t]*authorizedprincipalsfile ^[ \t]*authorizedkeysfile"` +
		` & echo ===CA===` +
		` & type "%ProgramData%\ssh\vault-ca.pub" "%ProgramData%\ssh\ops-ca.pub" 2>nul` +
		` & echo ===PRINCIPALS===` +
		` & type "%ProgramData%\ssh\auth_principals\administrators" "%ProgramData%\ssh\auth_principals\administrator" "%ProgramData%\ssh\auth_principals\root" 2>nul` +
		` & echo ===AUTHKEYS===` +
		` & type "%ProgramData%\ssh\administrators_authorized_keys" "%USERPROFILE%\.ssh\authorized_keys" 2>nul`}

// SSHCheckCommand returns the read-only guest agent command that inspects
// sshd and ops CA trust on a Windows or POSIX (Linux/FreeBSD) host.
func SSHCheckCommand(windows bool) []string {
	if windows {
		return slices.Clone(windowsSSHCheck)
	}
	return slices.Clone(posixSSHCheck)
}

func (p Planner) checkHostSSH(ctx context.Context, h EnvHost, nics []proxmox.GuestNIC) string {
	win := h.Notes.OS == ImageWindows || slices.ContainsFunc(nics, func(n proxmox.GuestNIC) bool {
		return strings.Contains(n.Name, "Loopback Pseudo-Interface")
	})
	pid, err := p.API.AgentRead(ctx, h.Node, h.VMID, SSHCheckCommand(win))
	if err != nil {
		var apiErr *proxmox.APIError
		if errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "disabled") {
			return "guest-exec disabled"
		}
		return ""
	}
	for {
		st, err := p.API.AgentExecStatus(ctx, h.Node, h.VMID, pid, maxSSHCheckOutput)
		if err != nil {
			return ""
		}
		if st.Exited {
			if st.ExitCode != 0 && strings.TrimSpace(st.Out) == "" {
				return ""
			}
			return CheckSSHOutput(st.Out, p.Cfg.Environments.OpsCAKey)
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(sshPollInterval):
		}
	}
}

var opsPrincipals = []string{"core-infrastructure", "competition-infrastructure"}

// CheckSSHOutput inspects the output of SSHCheckCommand and returns a short
// warning when sshd or the ops CA key isn't configured, or "" when healthy.
func CheckSSHOutput(out, opsCAKey string) string {
	if strings.TrimSpace(out) == "" {
		return ""
	}
	sections := map[string]string{}
	cur := "HEADER"
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case "===SSHD===":
			cur = "SSHD"
		case "===CA===":
			cur = "CA"
		case "===PRINCIPALS===":
			cur = "PRINCIPALS"
		case "===AUTHKEYS===":
			cur = "AUTHKEYS"
		default:
			sections[cur] += line + "\n"
		}
	}
	if !strings.Contains(sections["HEADER"], "sshd=running") {
		return "SSH not running"
	}
	var trustedCA, principalsFile string
	for _, line := range strings.Split(sections["SSHD"], "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		k, v := strings.ToLower(f[0]), f[1]
		switch k {
		case "permitrootlogin":
			if strings.EqualFold(v, "no") || strings.EqualFold(v, "forced-commands-only") {
				return "SSH root login off"
			}
		case "pubkeyauthentication":
			if strings.EqualFold(v, "no") {
				return "SSH pubkey auth off"
			}
		case "trustedusercakeys":
			if trustedCA == "" {
				trustedCA = v
			}
		case "authorizedprincipalsfile":
			if principalsFile == "" {
				principalsFile = v
			}
		}
	}
	var canonCA string
	if opsCAKey != "" {
		var err error
		if canonCA, err = config.ParseSSHPublicKey(opsCAKey); err != nil {
			return ""
		}
	}
	foundKey := false
	if trustedCA != "" && !strings.EqualFold(trustedCA, "none") && hasSSHKey(sections["CA"], canonCA) {
		foundKey = true
		if principalsFile != "" && !strings.EqualFold(principalsFile, "none") && hasOpsPrincipal(sections["PRINCIPALS"]) {
			return ""
		}
	}
	if ok, keyOnly := hasCertAuthority(sections["AUTHKEYS"], canonCA); ok {
		return ""
	} else if keyOnly {
		foundKey = true
	}
	if foundKey {
		return "missing ops CA principal"
	}
	return "missing ops CA key"
}

func hasSSHKey(text, canonCA string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, err := config.ParseSSHPublicKey(line); err == nil && (canonCA == "" || k == canonCA) {
			return true
		}
	}
	return false
}

func hasOpsPrincipal(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		p := strings.TrimSpace(line)
		if slices.Contains(opsPrincipals, p) {
			return true
		}
	}
	return false
}

func splitAuthorizedKeyOptions(line string) (opts, rest string) {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ' ', '\t':
			if !inQuote {
				return line[:i], strings.TrimSpace(line[i+1:])
			}
		}
	}
	return "", ""
}

func hasCertAuthority(text, canonCA string) (ok, keyOnly bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		opts, rest := splitAuthorizedKeyOptions(line)
		if !strings.Contains(opts, "cert-authority") {
			continue
		}
		k, err := config.ParseSSHPublicKey(rest)
		if err != nil || (canonCA != "" && k != canonCA) {
			continue
		}
		keyOnly = true
		_, after, hasPrincipals := strings.Cut(opts, `principals="`)
		if !hasPrincipals {
			return true, true
		}
		val, _, closed := strings.Cut(after, `"`)
		if !closed {
			continue
		}
		for _, p := range strings.Split(val, ",") {
			if slices.Contains(opsPrincipals, strings.TrimSpace(p)) {
				return true, true
			}
		}
	}
	return false, keyOnly
}
