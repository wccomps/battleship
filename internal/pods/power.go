package pods

// PowerAction is one way a power job changes its VMs' power.
type PowerAction struct {
	Value string // as jobs.Inputs.Action and Proxmox name it; the web app names and draws it
	// Leaves is the Proxmox status the action leaves a VM in; a VM already
	// in it is skipped. Reboot leaves none: it acts on every VM.
	Leaves string
}

// PowerActions are the power actions, in the order forms offer them.
var PowerActions = []PowerAction{
	{"start", "running"},
	{"shutdown", "stopped"},
	{"stop", "stopped"},
	{"reboot", ""},
}

// PowerActionOf returns the power action whose Value is action.
func PowerActionOf(action string) (PowerAction, bool) {
	for _, a := range PowerActions {
		if a.Value == action {
			return a, true
		}
	}
	return PowerAction{}, false
}

// IsPowerAction reports whether action is the Value of one of PowerActions.
func IsPowerAction(action string) bool {
	_, ok := PowerActionOf(action)
	return ok
}
