package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"unicode"
)

// Practice configures the session interface: the practice app deploys and
// tears down the session pods through it, each call carrying the principal's
// own Proxmox ticket. With every setting empty (the default) the interface
// is off; with any set, all are required.
type Practice struct {
	Pods string `toml:"pods"` // the pods it may act on, e.g. "51-59"
	// Principal is the one Proxmox user whose ticket it accepts, e.g.
	// practice@pve. Never a user of proxmox.realm: every job a volunteer
	// submits acts as a user of that realm, so the principal's jobs are told
	// from theirs by the user they act as.
	Principal string `toml:"principal"`
	Listen    string `toml:"listen"` // its own address, e.g. ":8081"; never web.listen
	// Key is what the practice app proves itself with. Overridden by
	// BATTLESHIP_PRACTICE_KEY.
	Key string `toml:"key"`
}

// MinPracticeKey is the shortest practice.key accepted.
const MinPracticeKey = 32

// maxPod is the highest pod number battleship can name (two digits).
const maxPod = 99

// Enabled reports whether the session interface is configured. Validate
// refuses a partly set [practice], so in a loaded config any setting means
// all of them.
func (p Practice) Enabled() bool { return p != Practice{} }

// PodRange is the first and last pod the interface may act on, for an
// Enabled and validated p.
func (p Practice) PodRange() (lo, hi int) {
	lo, hi, _ = ParseRange(p.Pods)
	return lo, hi
}

// String redacts the key so a config can be logged safely.
func (p Practice) String() string {
	key := ""
	if p.Key != "" {
		key = " Key:[redacted]"
	}
	return fmt.Sprintf("{Pods:%s Principal:%s Listen:%s%s}", p.Pods, p.Principal, p.Listen, key)
}

// GoString redacts the key for %#v.
func (p Practice) GoString() string { return "config.Practice" + p.String() }

// validatePractice checks [practice]: unset is fine, partly set is not.
func (c Config) validatePractice() []error {
	p := c.Practice
	if !p.Enabled() {
		return nil
	}
	const once = " once any [practice] setting is set"
	var errs []error

	if strings.TrimSpace(p.Pods) == "" {
		errs = append(errs, errors.New(`practice.pods is required`+once+` (the pods the session interface may act on, e.g. "51-59")`))
	} else if _, hi, err := ParseRange(p.Pods); err != nil {
		errs = append(errs, fmt.Errorf("practice.pods: %w", err))
	} else if hi > maxPod {
		errs = append(errs, fmt.Errorf("practice.pods must lie within 0-%d, the pods battleship can name", maxPod))
	}

	at := strings.LastIndex(p.Principal, "@")
	switch {
	case p.Principal == "":
		errs = append(errs, errors.New("practice.principal is required"+once+" (the Proxmox user the practice app logs in as, e.g. practice@pve)"))
	case at <= 0 || at == len(p.Principal)-1 ||
		strings.ContainsFunc(p.Principal, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		errs = append(errs, fmt.Errorf("practice.principal must be a Proxmox user as name@realm, e.g. practice@pve, not %q", p.Principal))
	case strings.EqualFold(p.Principal[at+1:], c.Proxmox.Realm):
		errs = append(errs, fmt.Errorf("practice.principal must not be in proxmox.realm (%s): volunteers' jobs act as users of that realm, and names there come from the identity provider; use a user of a Proxmox realm such as pve", c.Proxmox.Realm))
	}

	if p.Listen == "" {
		errs = append(errs, errors.New(`practice.listen is required`+once+` (the address only the practice app can reach, e.g. ":8081")`))
	} else if _, port, err := net.SplitHostPort(p.Listen); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("practice.listen must be host:port or :port, not %q", p.Listen))
	} else if sameSocket(p.Listen, c.Web.Listen) {
		errs = append(errs, errors.New("practice.listen must not be web.listen: the session interface is served apart from the volunteer pages, on an address only the practice app can reach"))
	}

	switch {
	case p.Key == "":
		errs = append(errs, fmt.Errorf("practice.key (or BATTLESHIP_PRACTICE_KEY) is required"+once+": %d or more random characters, the same as the practice app holds", MinPracticeKey))
	case len(p.Key) < MinPracticeKey:
		errs = append(errs, fmt.Errorf("practice.key must be at least %d characters", MinPracticeKey))
	}
	return errs
}

// sameSocket reports whether two host:port addresses would bind the same
// socket: equal ports, and equal hosts or either one every address.
func sameSocket(a, b string) bool {
	ha, pa, errA := net.SplitHostPort(a)
	hb, pb, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || pa != pb {
		return false
	}
	return ha == hb || anyAddress(ha) || anyAddress(hb)
}

func anyAddress(host string) bool {
	if host == "" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsUnspecified()
}
