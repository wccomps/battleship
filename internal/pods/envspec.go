package pods

import (
	"fmt"
	"hash/fnv"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
)

// Every environment's LAN: the router is .2, .3 is the team VPN endpoint,
// hosts take .4 to .254.
const (
	GatewayOctet   = 2
	FirstHostOctet = 4
	LastHostOctet  = 254
	// DNSRouter is the DNS choice that points hosts at the router.
	DNSRouter = "router"
	// MaxWindowsHostname is the NetBIOS name limit Rename-Computer enforces.
	MaxWindowsHostname = 15
)

// HostSpec is a host to add: its name, its base-image family, and the last
// octet of its address (0: Battleship picks one).
type HostSpec struct {
	Host   string `json:"host"`
	Family string `json:"family"`
	Octet  int    `json:"octet,omitempty"`
}

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckHostname checks a host name: one lower-case DNS label.
func CheckHostname(h string) error {
	if !labelRE.MatchString(h) {
		return fmt.Errorf("host name %q must be lower-case letters, digits and dashes (one DNS label)", h)
	}
	return nil
}

// CheckDomain checks an environment's domain: two or more DNS labels.
func CheckDomain(d string) error {
	labels := strings.Split(d, ".")
	if len(labels) < 2 || len(d) > 200 {
		return fmt.Errorf("domain %q must be two or more labels like kilo.alpha", d)
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return fmt.Errorf("domain %q: %q isn't a lower-case DNS label", d, l)
		}
	}
	return nil
}

// ParseHostSpec reads "host=family" or "host=family:octet".
func ParseHostSpec(s string) (HostSpec, error) {
	host, rest, ok := strings.Cut(s, "=")
	if !ok {
		return HostSpec{}, fmt.Errorf("%q: write a host as name=image, e.g. dc=win2022 or dc=win2022:10", strings.TrimSpace(s))
	}
	family, octet, hasOctet := strings.Cut(rest, ":")
	h := HostSpec{Host: strings.TrimSpace(host), Family: strings.TrimSpace(family)}
	if err := CheckHostname(h.Host); err != nil {
		return HostSpec{}, err
	}
	if h.Family == "" {
		return HostSpec{}, fmt.Errorf("%q: name the base image after =", strings.TrimSpace(s))
	}
	if err := CheckFamily(h.Family); err != nil {
		return HostSpec{}, err
	}
	if hasOctet {
		n, err := strconv.Atoi(strings.TrimSpace(octet))
		if err != nil || n < FirstHostOctet || n > LastHostOctet {
			return HostSpec{}, fmt.Errorf("%q: the address must be %d-%d (.%d is the router, .3 the team VPN)",
				strings.TrimSpace(s), FirstHostOctet, LastHostOctet, GatewayOctet)
		}
		h.Octet = n
	}
	return h, nil
}

func (h HostSpec) String() string {
	s := h.Host + "=" + h.Family
	if h.Octet != 0 {
		s += ":" + strconv.Itoa(h.Octet)
	}
	return s
}

// Notes is the battleship line in an environment VM's Proxmox notes: what
// Battleship knows about it while it is stopped, and after its base image
// is gone.
type Notes struct {
	IP   netip.Addr `json:"ip,omitzero"`
	OS   string     `json:"os,omitempty"` // windows or linux: hosts configured through the agent
	Base string     `json:"base,omitempty"`
	DNS  string     `json:"dns,omitempty"` // DNSRouter or a host name
	// Source is the VMID a base image was registered from, for registering
	// its next version from the same VM.
	Source string `json:"source,omitempty"`
}

const notesPrefix = "battleship:"

// ParseNotes finds and reads the battleship line in desc.
func ParseNotes(desc string) (Notes, bool) {
	for _, line := range strings.Split(desc, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), notesPrefix)
		if !ok {
			continue
		}
		var n Notes
		for _, f := range strings.Fields(rest) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "ip":
				n.IP, _ = netip.ParseAddr(v)
			case "os":
				n.OS = v
			case "base":
				n.Base = v
			case "dns":
				n.DNS = v
			case "source":
				n.Source = v
			}
		}
		return n, true
	}
	return Notes{}, false
}

// maxUserNotes caps the notes UserNotes keeps, in runes.
const maxUserNotes = 4000

// UserNotes is desc without battleship's lines: what people wrote, such as
// logins. It is text for people; pages escape it like any other text. Format
// characters (bidi overrides, zero widths) are dropped so notes can't
// disguise what they say, and a NUL reads as U+FFFD, as pages show it and
// as a stored job can hold it.
func UserNotes(desc string) string {
	s := userText(desc)
	if r := []rune(s); len(r) > maxUserNotes {
		s = string(r[:maxUserNotes]) + "…"
	}
	return s
}

// userText is UserNotes without its cap.
func userText(desc string) string {
	desc = strings.ReplaceAll(dropFormat(desc), "\x00", "\ufffd")
	var keep []string
	for _, line := range strings.Split(desc, "\n") {
		if !isNotesLine(line) {
			keep = append(keep, strings.TrimRight(line, " \t\r"))
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// Checked drops the values a guest script can't take. Users can edit notes,
// and IP reaches scripts that run as root or SYSTEM: it must be a host
// address of lan, or the host is treated as having none.
func (n Notes) Checked(lan config.LAN) Notes {
	if o, ok := lan.Octet(n.IP); !ok || o < FirstHostOctet || o > LastHostOctet {
		n.IP = netip.Addr{}
	}
	if n.OS != ImageWindows && n.OS != ImageLinux {
		n.OS = ""
	}
	if n.DNS != DNSRouter && CheckHostname(n.DNS) != nil {
		n.DNS = ""
	}
	return n
}

func (n Notes) String() string {
	parts := []string{notesPrefix}
	for _, kv := range [][2]string{{"ip", AddrText(n.IP)}, {"os", n.OS}, {"base", n.Base}, {"dns", n.DNS}, {"source", n.Source}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

// WithNotes replaces desc's battleship line with n's, or appends one.
func WithNotes(desc string, n Notes) string {
	lines := strings.Split(desc, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), notesPrefix) {
			lines[i] = n.String()
			return strings.Join(lines, "\n")
		}
	}
	if strings.TrimSpace(desc) == "" {
		return n.String()
	}
	return strings.TrimRight(desc, "\n") + "\n" + n.String()
}

// PickOctet picks an unused host octet for host in domain. The choice
// looks random but is the same every time it is planned, so a preview and
// the job's re-plan agree.
func PickOctet(domain, host string, used map[int]bool) (int, bool) {
	h := fnv.New32a()
	h.Write([]byte(domain + "/" + host))
	n := LastHostOctet - FirstHostOctet + 1
	start := int(h.Sum32() % uint32(n))
	for i := range n {
		o := FirstHostOctet + (start+i)%n
		if !used[o] {
			return o, true
		}
	}
	return 0, false
}

func AddrText(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}
