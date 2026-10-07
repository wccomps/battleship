package pods

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/config"
)

// Expand replaces {team} and {host} in a config pattern.
func Expand(pattern, team, host string) string {
	return strings.NewReplacer("{team}", team, "{host}", host).Replace(pattern)
}

// Hostname is the part of a master or template name before the first dot:
// "teak.tango.delta.tpl" -> "teak".
func Hostname(name string) string {
	host, _, _ := strings.Cut(name, ".")
	return host
}

// Naming builds VM names, pool names, and VMIDs from config patterns.
type Naming struct {
	cfg    config.Naming
	nameRE *regexp.Regexp
}

func NewNaming(cfg config.Naming) Naming {
	return Naming{cfg: cfg, nameRE: patternRE(cfg.VMName)}
}

// patternRE matches what a config pattern expands to: {team} as two digits,
// {host} as anything.
func patternRE(pattern string) *regexp.Regexp {
	re := strings.NewReplacer(`\{team\}`, `(?P<team>\d{2})`, `\{host\}`, `(?P<host>.+)`).Replace(regexp.QuoteMeta(pattern))
	return regexp.MustCompile("^" + re + "$")
}

func (n Naming) VMName(team, host string) string { return Expand(n.cfg.VMName, team, host) }

// Pool generates a pool name for a team. {host} is not used.
func (n Naming) Pool(team string) string { return Expand(n.cfg.Pool, team, "") }

// CloneVMID is base + team*stride + templateVMID%100, e.g. team 01 from
// template 9005 -> 10105. The planner ensures a plan's templates have
// distinct VMID%100.
func (n Naming) CloneVMID(team string, templateVMID int) int {
	t, err := strconv.Atoi(team)
	if err != nil {
		panic(fmt.Sprintf("CloneVMID: team %q is not a number", team))
	}
	if t < 0 || t > 99 {
		panic(fmt.Sprintf("CloneVMID: team %q is out of range (0-99)", team))
	}
	return n.cfg.CloneVMIDBase + t*n.cfg.CloneVMIDTeamStride + templateVMID%100
}

// TeamOfCloneVMID is the two-digit team whose clone VMIDs (the first 100 of
// its stride) include vmid.
func (n Naming) TeamOfCloneVMID(vmid int) (string, bool) {
	off := vmid - n.cfg.CloneVMIDBase
	stride := n.cfg.CloneVMIDTeamStride
	if off < 0 || off/stride > 99 || off%stride > 99 {
		return "", false
	}
	return fmt.Sprintf("%02d", off/stride), true
}

func (n Naming) TemplateName(masterName string) string { return masterName + n.cfg.TemplateSuffix }

func (n Naming) IsTemplateName(name string) bool {
	return strings.HasSuffix(name, n.cfg.TemplateSuffix)
}

// TemplateVMID is the preferred template VMID for a master, before collisions.
func (n Naming) TemplateVMID(masterVMID int) int { return n.cfg.TemplateVMIDBase + masterVMID%100 }

// ParseVMName extracts team and host from a team VM name, accepting only
// names VMName would produce, so "team7-x" is never team 07.
func (n Naming) ParseVMName(name string) (team, host string, ok bool) {
	m := n.nameRE.FindStringSubmatch(name)
	if m == nil {
		return "", "", false
	}
	num, err := strconv.Atoi(m[n.nameRE.SubexpIndex("team")])
	if err != nil {
		return "", "", false
	}
	team = FormatTeam(num)
	host = m[n.nameRE.SubexpIndex("host")]
	if n.VMName(team, host) != name {
		return "", "", false
	}
	return team, host, true
}
