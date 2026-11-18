package pods

import (
	"regexp"
	"slices"
	"strings"
)

// An environment without machines is recorded in its slot pool's Proxmox
// comment as the token "[env <domain>]"; the rest of the comment is the
// admins'.
var markerRE = regexp.MustCompile(`\s*\[env ([^\]]*)\]`)

// EnvMarker is the domain comment's marker records, or "" when it has none
// or names no valid domain.
func EnvMarker(comment string) string {
	m := markerRE.FindStringSubmatch(comment)
	if m == nil || CheckDomain(m[1]) != nil {
		return ""
	}
	return m[1]
}

// WithEnvMarker is comment with its marker, if any, replaced by domain's.
func WithEnvMarker(comment, domain string) string {
	rest := strings.TrimSpace(markerRE.ReplaceAllString(comment, ""))
	if rest == "" {
		return "[env " + domain + "]"
	}
	return rest + " [env " + domain + "]"
}

// WithoutEnvMarker is comment less its marker if that names domain, else
// comment as it is.
func WithoutEnvMarker(comment, domain string) string {
	if EnvMarker(comment) != domain {
		return comment
	}
	return strings.TrimSpace(markerRE.ReplaceAllString(comment, ""))
}

// IsRecord reports whether it is a record or clear-record item: one on its
// Pool's comment, not a VM.
func (it Item) IsRecord() bool {
	return slices.Contains(it.Steps, StepRecord) || slices.Contains(it.Steps, StepClearRecord)
}

// recordItem writes domain's marker on slot's pool (StepRecord) or takes it
// off (StepClearRecord); a clear runs Last.
func recordItem(slot Slot, domain string, step Step) Item {
	return Item{Name: slot.Pool, Pool: slot.Pool, Record: domain, Steps: []Step{step}, Last: step == StepClearRecord}
}
