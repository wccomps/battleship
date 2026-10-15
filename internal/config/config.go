// Package config loads the battleship TOML configuration.
//
// Patterns use {team} (two-digit team number) and {host} (template hostname)
// placeholders.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Proxmox      Proxmox      `toml:"proxmox"`
	Naming       Naming       `toml:"naming"`
	Network      Network      `toml:"network"`
	Deploy       Deploy       `toml:"deploy"`
	Teardown     Teardown     `toml:"teardown"`
	Images       Images       `toml:"images"`
	Environments Environments `toml:"environments"`
	Concurrency  Concurrency  `toml:"concurrency"`
	Retry        Retry        `toml:"retry"`
	Database     Database     `toml:"database"`
	Jobs         Jobs         `toml:"jobs"`
	Web          Web          `toml:"web"`
	OIDC         OIDC         `toml:"oidc"`
}

type Proxmox struct {
	URL string `toml:"url"` // e.g. https://192.0.2.123:8006
	// URLs are several cluster nodes, used one at a time with failover.
	// Exactly one of URL and URLs is set.
	URLs               []string `toml:"urls"`
	InsecureSkipVerify bool     `toml:"insecure_skip_verify"` // default false; serve needs ca_file or this set to true
	// CAFile is a PEM bundle to verify Proxmox against, e.g.
	// /etc/pve/pve-root-ca.pem. Not allowed with insecure_skip_verify.
	CAFile string `toml:"ca_file"`
	// RootCAs is CAFile's certificates, read by Load.
	RootCAs *x509.CertPool `toml:"-"`
	// Realm is the Proxmox OpenID realm users sign in to Proxmox through.
	Realm string `toml:"realm"`
	// TicketRenewAfter is the ticket age at which battleship renews it.
	// Tickets last 2h and can only be renewed while valid.
	TicketRenewAfter time.Duration `toml:"ticket_renew_after"`
	// SDNZone holds the team bridges' vnets; a deploy needs SDN.Use on them.
	SDNZone string `toml:"sdn_zone"`
	// TicketMaxAge is how long a Proxmox login lasts through renewals.
	// Renewal skips the IdP, so group changes apply only at the next login.
	TicketMaxAge time.Duration `toml:"ticket_max_age"`
}

type Naming struct {
	VMName              string `toml:"vm_name"`
	Pool                string `toml:"pool"`
	CloneVMIDBase       int    `toml:"clone_vmid_base"`
	CloneVMIDTeamStride int    `toml:"clone_vmid_team_stride"`
	TemplateVMIDBase    int    `toml:"template_vmid_base"`
	TemplateSuffix      string `toml:"template_suffix"`
}

type Network struct {
	ExtBridge string `toml:"ext_bridge"`
	IntBridge string `toml:"int_bridge"`
	ExtSubnet string `toml:"ext_subnet"` // first three octets, e.g. 10.50.1{team}
	CICustom  string `toml:"cicustom"`   // empty disables
}

type Deploy struct {
	Storage       string `toml:"storage"`
	Linked        bool   `toml:"linked"`
	MasterTag     string `toml:"master_tag"`
	DiskMBpsRead  int    `toml:"disk_mbps_rd"`
	DiskMBpsWrite int    `toml:"disk_mbps_wr"`
	SnapshotName  string `toml:"snapshot_name"` // the baseline a deploy takes
	// BaselinePatterns are path.Match globs for snapshots that count as the
	// baseline when a VM lacks SnapshotName; a reset uses the newest match.
	BaselinePatterns []string `toml:"baseline_patterns"`
	GPUVGA           string   `toml:"gpu_vga"` // templates with this vga value are pinned to their node
}

// Concurrency limits Proxmox work. Workers is per job; ConfigCalls and
// TemplateBuilds are per process. With a database, Clones, Deletes and
// serialized storage operations are capped across processes.
type Concurrency struct {
	Workers int `toml:"workers"` // VMs processed at once
	// Clones caps concurrent clone tasks; 0 means no cap. Stock Proxmox takes
	// the clone storage's cluster lock for every clone, so they run about one
	// a second and too many queued time out: set 8 there.
	Clones      int `toml:"clones"`
	ConfigCalls int `toml:"config_calls"` // concurrent API calls from jobs and the drift scan, clone POSTs included
	// TemplateBuilds caps concurrent template builds; each also takes a
	// clone slot.
	TemplateBuilds int `toml:"template_builds"`
	// Deletes caps concurrent VM destroys. Each takes Proxmox's cluster-wide
	// user.cfg lock; too many time out on it and leave VMs half-deleted.
	Deletes int `toml:"deletes"`
}

// Images configures base images: templates environments are cloned from.
type Images struct {
	Pool      string `toml:"pool"`       // where base images live; an admin creates it
	VMIDRange string `toml:"vmid_range"` // e.g. "8000-8999"
	// EditBridge is the net0 bridge of a working copy opened for editing,
	// which needs a network with internet access to patch. EditZone is its SDN
	// zone, or "" if it isn't an SDN vnet.
	EditBridge string `toml:"edit_bridge"`
	EditZone   string `toml:"edit_zone"`
}

// Environments configures the dev slots scenarios are built in. Patterns
// use {slot}, the slot number as two digits.
type Environments struct {
	Slots     string `toml:"slots"` // e.g. "1-10"
	Pool      string `toml:"pool"`
	IntBridge string `toml:"int_bridge"`
	ExtBridge string `toml:"ext_bridge"`
	SDNZone   string `toml:"sdn_zone"` // the zone of the slot bridges
	// VMIDBase: slot NN's VMs get VMIDs VMIDBase+NN*100 to +99.
	VMIDBase int `toml:"vmid_base"`
	// Subnet is the first three octets of every environment's LAN.
	Subnet       string        `toml:"subnet"`
	AgentTimeout time.Duration `toml:"agent_timeout"`
}

// ParseRange reads "lo-hi" with 0 <= lo <= hi.
func ParseRange(s string) (lo, hi int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if ok {
		lo, err = strconv.Atoi(strings.TrimSpace(a))
		if err == nil {
			hi, err = strconv.Atoi(strings.TrimSpace(b))
		}
	}
	if !ok || err != nil || lo < 0 || hi < lo {
		return 0, 0, fmt.Errorf("%q is not a range like 1-10", s)
	}
	return lo, hi, nil
}

// Teardown configures how team VMs are taken down before they are deleted.
type Teardown struct {
	// ShutdownTimeout is how long a VM gets to shut down before a hard stop.
	// A clean shutdown lets a router release its DHCP lease for the next deploy.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`
}

type Retry struct {
	TransientPatterns []string      `toml:"transient_patterns"` // case-insensitive substrings
	Attempts          int           `toml:"attempts"`           // tries per API call; also the most times a task that failed transiently is started
	InitialBackoff    time.Duration `toml:"initial_backoff"`
	MaxBackoff        time.Duration `toml:"max_backoff"`
	Rounds            int           `toml:"rounds"`      // end-of-job retry rounds
	RoundPause        time.Duration `toml:"round_pause"` // pause before each round
	TaskPoll          time.Duration `toml:"task_poll"`   // longest wait between task status polls; they start at 100ms and double
}

// Database stores jobs. serve and -queue need it; when set, every CLI run
// is queued as a job.
type Database struct {
	URL string `toml:"url"` // postgres://...; overridden by BATTLESHIP_DATABASE_URL
	// MaxConns caps the main connection pool (min 4, default 16). A process
	// uses up to MaxConns+4 connections; budget that against max_connections.
	MaxConns int `toml:"max_conns"`
	// SealKey encrypts job credentials in the database; every process that
	// submits or runs jobs needs the same key. Overridden by BATTLESHIP_SEAL_KEY.
	SealKey string `toml:"seal_key"`
}

// String redacts the password in URL and the seal key so a config can be
// logged safely.
func (d Database) String() string {
	key := ""
	if d.SealKey != "" {
		key = " SealKey:[redacted]"
	}
	return fmt.Sprintf("{URL:%s MaxConns:%d%s}", redactURL(d.URL), d.MaxConns, key)
}

// MinSealKey is the shortest database.seal_key accepted.
const MinSealKey = 32

// RequireSealKey reports an error if no seal key is configured.
func (c Config) RequireSealKey() error {
	if c.Database.SealKey == "" {
		return fmt.Errorf("database.seal_key (or BATTLESHIP_SEAL_KEY) is required to keep jobs' Proxmox credentials: %d or more random characters, the same in every process", MinSealKey)
	}
	return nil
}

// GoString redacts the password for %#v.
func (d Database) GoString() string { return "config.Database" + d.String() }

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		if err != nil && raw != "" {
			return "[unparseable, redacted]"
		}
		return raw
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "redacted")
	}
	return u.String()
}

type Jobs struct {
	Heartbeat  time.Duration `toml:"heartbeat"`   // how often a running job proves it's alive
	StaleAfter time.Duration `toml:"stale_after"` // a running job silent this long is marked interrupted
	Poll       time.Duration `toml:"poll"`        // how often the CLI checks a job it follows
	// CancelGrace is how long a canceled step waits for a Proxmox task it
	// already started, so the job records what it did.
	CancelGrace time.Duration `toml:"cancel_grace"`
}

// Default returns the values that match the current competition setup.
func Default() Config {
	return Config{
		Proxmox: Proxmox{Realm: "auth.example.org", SDNZone: "teams", TicketRenewAfter: time.Hour, TicketMaxAge: 12 * time.Hour},
		Naming: Naming{
			VMName:              "team{team}-{host}",
			Pool:                "pool-{team}",
			CloneVMIDBase:       10000,
			CloneVMIDTeamStride: 100,
			TemplateVMIDBase:    9000,
			TemplateSuffix:      ".tpl",
		},
		Network: Network{
			ExtBridge: "ext{team}",
			IntBridge: "int{team}",
			ExtSubnet: "10.50.1{team}",
			CICustom:  "vendor=competitions:snippets/ssh-keys.yaml",
		},
		Deploy: Deploy{
			Storage:          "competitions",
			Linked:           true,
			MasterTag:        "dev",
			DiskMBpsRead:     300,
			DiskMBpsWrite:    300,
			SnapshotName:     "initial",
			BaselinePatterns: []string{"fresh_clone_*"},
			GPUVGA:           "virtio-gl",
		},
		Images: Images{Pool: "dev-templates", VMIDRange: "8000-8999", EditBridge: "vmbr0"},
		Environments: Environments{
			Slots: "1-10", Pool: "dev-{slot}", IntBridge: "devint{slot}", ExtBridge: "devext{slot}",
			SDNZone: "devnet", VMIDBase: 2000, Subnet: "192.168.100", AgentTimeout: 15 * time.Minute,
		},
		Concurrency: Concurrency{Workers: 20, ConfigCalls: 5, TemplateBuilds: 4, Deletes: 4},
		Teardown:    Teardown{ShutdownTimeout: 60 * time.Second},
		Jobs:        Jobs{Heartbeat: 10 * time.Second, StaleAfter: 60 * time.Second, Poll: 2 * time.Second, CancelGrace: 30 * time.Second},
		Database:    Database{MaxConns: 16},
		Retry: Retry{
			TransientPatterns: []string{
				"file exists",
				"does not exist",
				"got no worker upid",
				"can't lock file",
				"is locked",
				"timeout",
			},
			// 1+2+4+8+16+30s of waits outlast the up to 60s a node can miss a
			// disk folder another node recreated (NFS attribute cache).
			Attempts:       7,
			InitialBackoff: time.Second,
			MaxBackoff:     30 * time.Second,
			Rounds:         1,
			RoundPause:     2 * time.Second,
			TaskPoll:       2 * time.Second,
		},
		Web:  defaultWeb(),
		OIDC: defaultOIDC(),
	}
}

// String shows the Proxmox settings on one line.
func (p Proxmox) String() string {
	s := "{URL:" + p.URL
	if len(p.URLs) > 0 {
		s = "{URLs:[" + strings.Join(p.URLs, " ") + "]"
	}
	s += fmt.Sprintf(" Realm:%s InsecureSkipVerify:%t", p.Realm, p.InsecureSkipVerify)
	if p.CAFile != "" {
		s += " CAFile:" + p.CAFile
	}
	return s + "}"
}

// Endpoints is the Proxmox API URLs in the order to try them.
func (p Proxmox) Endpoints() []string {
	if len(p.URLs) > 0 {
		return p.URLs
	}
	if p.URL != "" {
		return []string{p.URL}
	}
	return nil
}

// ReadCAFile reads a PEM bundle, failing if it holds no certificate.
func ReadCAFile(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err // names the file
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s holds no PEM certificates", path)
	}
	return pool, nil
}

// GoString is String for %#v.
func (p Proxmox) GoString() string { return "config.Proxmox" + p.String() }

// removedKeys are retired settings, with why. A config that sets one fails
// to load, so an old config can't silently keep e.g. a service token.
var removedKeys = map[string]string{
	"proxmox.token_id":            removedToken,
	"proxmox.token_secret":        removedToken,
	"roles.operators":             removedToken,
	"roles.leads":                 removedToken,
	"web.teams":                   "Battleship shows the teams that have VMs; delete the line",
	"concurrency.clones_per_node": "Clones are now capped across the cluster when there is a database; use concurrency.clones",
	"oidc.groups_claim":           "Battleship doesn't use groups; delete the line",
}

const removedToken = "Battleship holds no Proxmox token or roles of its own. " +
	"Every Proxmox call is made as the person who asked, and Proxmox's ACLs decide what each may do; delete these keys (see the README)"

// Load reads path over the defaults and applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		var keys, reasons []string
		gone := map[string][]string{} // by why they were removed
		var errs []error
		for _, k := range undecoded {
			why, ok := removedKeys[k.String()]
			switch {
			case !ok:
				keys = append(keys, k.String())
			case gone[why] == nil:
				reasons = append(reasons, why)
				fallthrough
			default:
				gone[why] = append(gone[why], k.String())
			}
		}
		for _, why := range reasons {
			errs = append(errs, fmt.Errorf("reading %s: these settings were removed: %s. %s", path, strings.Join(gone[why], ", "), why))
		}
		if len(keys) > 0 {
			errs = append(errs, fmt.Errorf("reading %s: unknown keys: %s", path, strings.Join(keys, ", ")))
		}
		return Config{}, errors.Join(errs...)
	}
	if s := os.Getenv("BATTLESHIP_DATABASE_URL"); s != "" {
		cfg.Database.URL = s
	}
	if s := os.Getenv("BATTLESHIP_SEAL_KEY"); s != "" {
		cfg.Database.SealKey = s
	}
	if s := os.Getenv("BATTLESHIP_OIDC_ISSUER"); s != "" {
		cfg.OIDC.Issuer = s
	}
	if s := os.Getenv("BATTLESHIP_OIDC_CLIENT_ID"); s != "" {
		cfg.OIDC.ClientID = s
	}
	if s := os.Getenv("BATTLESHIP_OIDC_CLIENT_SECRET"); s != "" {
		cfg.OIDC.ClientSecret = s
	}
	// Both https://battleship.example.org and https://battleship.example.org/ mean the site root.
	cfg.Web.BaseURL = strings.TrimSuffix(cfg.Web.BaseURL, "/")
	var caErr error
	if cfg.Proxmox.CAFile != "" {
		if cfg.Proxmox.RootCAs, caErr = ReadCAFile(cfg.Proxmox.CAFile); caErr != nil {
			caErr = fmt.Errorf("proxmox.ca_file: %w", caErr)
		}
	}
	if err := errors.Join(caErr, cfg.Validate()); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error

	// Proxmox: url or urls (not both), each http(s) with a host
	switch {
	case c.Proxmox.URL != "" && len(c.Proxmox.URLs) > 0:
		errs = append(errs, errors.New("proxmox.url and proxmox.urls can't both be set; list every node in urls"))
	case c.Proxmox.URL == "" && len(c.Proxmox.URLs) == 0:
		errs = append(errs, errors.New("proxmox.url or proxmox.urls is required"))
	case c.Proxmox.URL != "":
		if !httpURL(c.Proxmox.URL) {
			errs = append(errs, errors.New("proxmox.url must be an http(s) URL with a host and nothing after it, e.g. https://192.0.2.123:8006"))
		}
	default:
		seen := map[string]int{}
		for i, raw := range c.Proxmox.URLs {
			if !httpURL(raw) {
				errs = append(errs, fmt.Errorf("proxmox.urls[%d] must be an http(s) URL with a host and nothing after it, e.g. https://192.0.2.123:8006", i))
				continue
			}
			key := strings.TrimRight(raw, "/")
			if j, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf("proxmox.urls[%d] repeats proxmox.urls[%d]", i, j))
				continue
			}
			seen[key] = i
		}
	}

	if c.Proxmox.CAFile != "" {
		if c.Proxmox.InsecureSkipVerify {
			errs = append(errs, errors.New("proxmox.ca_file and proxmox.insecure_skip_verify = true can't both be set: "+
				"ca_file checks Proxmox's certificate and insecure_skip_verify turns the check off; remove insecure_skip_verify (or set it to false)"))
		}
	}

	if strings.TrimSpace(c.Proxmox.SDNZone) == "" {
		errs = append(errs, errors.New("proxmox.sdn_zone is required (the SDN zone of the team bridges, e.g. teams)"))
	}
	if strings.TrimSpace(c.Proxmox.Realm) == "" {
		errs = append(errs, errors.New("proxmox.realm is required (the Proxmox OpenID realm, e.g. auth.example.org)"))
	}
	// Renew with time to spare before the 2h ticket lapses.
	if c.Proxmox.TicketRenewAfter < 5*time.Minute || c.Proxmox.TicketRenewAfter > 100*time.Minute {
		errs = append(errs, errors.New("proxmox.ticket_renew_after must be between 5m and 1h40m (tickets last 2h and can only be renewed while valid)"))
	}
	if c.Proxmox.TicketMaxAge < c.Proxmox.TicketRenewAfter {
		errs = append(errs, errors.New("proxmox.ticket_max_age must be at least proxmox.ticket_renew_after"))
	}
	if c.Database.SealKey != "" && len(c.Database.SealKey) < MinSealKey {
		errs = append(errs, fmt.Errorf("database.seal_key must be at least %d characters", MinSealKey))
	}

	// Fixed order for deterministic errors.
	teamPatterns := []struct {
		name string
		val  string
	}{
		{"naming.vm_name", c.Naming.VMName},
		{"naming.pool", c.Naming.Pool},
		{"network.ext_bridge", c.Network.ExtBridge},
		{"network.int_bridge", c.Network.IntBridge},
		{"network.ext_subnet", c.Network.ExtSubnet},
	}
	for _, p := range teamPatterns {
		if !strings.Contains(p.val, "{team}") {
			errs = append(errs, fmt.Errorf("%s must contain {team}", p.name))
		}
	}

	// A second {host} could not be told from part of the first when a name is
	// read back, so such VMs would vanish from the grid.
	if strings.Count(c.Naming.VMName, "{host}") != 1 {
		errs = append(errs, errors.New("naming.vm_name must contain {host} once"))
	}

	if c.Naming.TemplateSuffix == "" {
		errs = append(errs, errors.New("naming.template_suffix is required"))
	}

	if c.Naming.CloneVMIDTeamStride < 100 {
		errs = append(errs, errors.New("naming.clone_vmid_team_stride must be at least 100 (clone VMIDs use template VMID % 100)"))
	}

	if c.Deploy.Storage == "" {
		errs = append(errs, errors.New("deploy.storage is required"))
	}
	if strings.TrimSpace(c.Deploy.MasterTag) == "" {
		errs = append(errs, errors.New("deploy.master_tag is required"))
	}
	if c.Deploy.SnapshotName == "" {
		errs = append(errs, errors.New("deploy.snapshot_name is required"))
	}
	for _, p := range c.Deploy.BaselinePatterns {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, errors.New("deploy.baseline_patterns must not contain blank entries"))
		} else if _, err := path.Match(p, ""); err != nil {
			errs = append(errs, fmt.Errorf("deploy.baseline_patterns: %q is not a valid glob: %w", p, err))
		}
	}

	if c.Deploy.DiskMBpsRead < 1 {
		errs = append(errs, errors.New("deploy.disk_mbps_rd must be at least 1"))
	}
	if c.Deploy.DiskMBpsWrite < 1 {
		errs = append(errs, errors.New("deploy.disk_mbps_wr must be at least 1"))
	}

	if c.Concurrency.Workers < 1 {
		errs = append(errs, errors.New("concurrency.workers must be at least 1"))
	}
	if c.Concurrency.Clones < 0 {
		errs = append(errs, errors.New("concurrency.clones must be 0 (no cap) or more"))
	}
	if c.Concurrency.ConfigCalls < 1 {
		errs = append(errs, errors.New("concurrency.config_calls must be at least 1"))
	}
	if c.Concurrency.TemplateBuilds < 1 {
		errs = append(errs, errors.New("concurrency.template_builds must be at least 1"))
	}
	if c.Concurrency.Deletes < 1 {
		errs = append(errs, errors.New("concurrency.deletes must be at least 1"))
	}

	// Teardown: Proxmox takes the timeout in whole seconds.
	if c.Teardown.ShutdownTimeout < time.Second {
		errs = append(errs, errors.New("teardown.shutdown_timeout must be at least 1s"))
	}

	const minDuration = 100 * time.Millisecond
	if c.Retry.TaskPoll < minDuration {
		errs = append(errs, errors.New("retry.task_poll must be at least 100ms"))
	}
	if c.Retry.InitialBackoff < minDuration {
		errs = append(errs, errors.New("retry.initial_backoff must be at least 100ms"))
	}
	if c.Retry.MaxBackoff < c.Retry.InitialBackoff {
		errs = append(errs, errors.New("retry.max_backoff must be at least retry.initial_backoff"))
	}

	if c.Retry.Rounds < 0 {
		errs = append(errs, errors.New("retry.rounds must not be negative"))
	}
	if c.Retry.RoundPause < 0 {
		errs = append(errs, errors.New("retry.round_pause must not be negative"))
	}

	if c.Retry.Attempts < 1 {
		errs = append(errs, errors.New("retry.attempts must be at least 1"))
	}

	// A worker stops its job after StaleAfter/2 without a heartbeat; 5 beats
	// per StaleAfter survive one failed beat, 4 would not.
	if c.Jobs.Heartbeat < time.Second {
		errs = append(errs, errors.New("jobs.heartbeat must be at least 1s"))
	}
	if c.Jobs.StaleAfter < 5*c.Jobs.Heartbeat {
		errs = append(errs, errors.New("jobs.stale_after must be at least 5 times jobs.heartbeat"))
	}
	if c.Jobs.Poll < 100*time.Millisecond {
		errs = append(errs, errors.New("jobs.poll must be at least 100ms"))
	}
	if c.Jobs.CancelGrace < 0 || c.Jobs.CancelGrace > 5*time.Minute {
		errs = append(errs, errors.New("jobs.cancel_grace must be between 0s and 5m"))
	}

	// Database: room for a worker's job and a page or two at once.
	if c.Database.MaxConns < 4 {
		errs = append(errs, errors.New("database.max_conns must be at least 4"))
	}

	errs = append(errs, c.validateCatalog()...)
	errs = append(errs, c.validateWeb()...)
	return errors.Join(errs...)
}

// validateCatalog checks [images] and [environments]: their VMID blocks
// must not overlap each other, the templates' or the team clones'.
func (c Config) validateCatalog() []error {
	var errs []error
	if strings.TrimSpace(c.Images.Pool) == "" {
		errs = append(errs, errors.New("images.pool is required (the pool base images live in)"))
	}
	if strings.TrimSpace(c.Images.EditBridge) == "" {
		errs = append(errs, errors.New("images.edit_bridge is required (the bridge working copies are patched on)"))
	}
	imgLo, imgHi, err := ParseRange(c.Images.VMIDRange)
	if err != nil {
		errs = append(errs, fmt.Errorf("images.vmid_range: %w", err))
	}
	slotLo, slotHi, serr := ParseRange(c.Environments.Slots)
	switch {
	case serr != nil:
		errs = append(errs, fmt.Errorf("environments.slots: %w", serr))
	case slotLo < 1 || slotHi > 99:
		errs = append(errs, errors.New("environments.slots must be within 1-99"))
	}
	for _, p := range []struct{ name, val string }{
		{"environments.pool", c.Environments.Pool},
		{"environments.int_bridge", c.Environments.IntBridge},
		{"environments.ext_bridge", c.Environments.ExtBridge},
	} {
		if !strings.Contains(p.val, "{slot}") {
			errs = append(errs, fmt.Errorf("%s must contain {slot}", p.name))
		}
	}
	if strings.TrimSpace(c.Environments.SDNZone) == "" {
		errs = append(errs, errors.New("environments.sdn_zone is required (the SDN zone of the slot bridges)"))
	}
	if !subnetRE.MatchString(c.Environments.Subnet) {
		errs = append(errs, errors.New("environments.subnet must be three octets, e.g. 192.168.100"))
	}
	if c.Environments.AgentTimeout < time.Minute {
		errs = append(errs, errors.New("environments.agent_timeout must be at least 1m (Windows takes minutes to boot after sysprep)"))
	}
	if c.Environments.VMIDBase < 100 {
		errs = append(errs, errors.New("environments.vmid_base must be at least 100"))
	} else if err == nil && serr == nil {
		envLo, envHi := c.Environments.VMIDBase+slotLo*100, c.Environments.VMIDBase+slotHi*100+99
		tplLo, tplHi := c.Naming.TemplateVMIDBase, c.Naming.TemplateVMIDBase+99
		if overlapsRange(envLo, envHi, imgLo, imgHi) || overlapsRange(envLo, envHi, tplLo, tplHi) || envHi >= c.Naming.CloneVMIDBase {
			errs = append(errs, fmt.Errorf("environments.vmid_base gives slots VMIDs %d-%d, which overlap images.vmid_range, the templates (%d-%d) or team clones (from %d)",
				envLo, envHi, tplLo, tplHi, c.Naming.CloneVMIDBase))
		}
	}
	if err == nil && (overlapsRange(imgLo, imgHi, c.Naming.TemplateVMIDBase, c.Naming.TemplateVMIDBase+99) || imgHi >= c.Naming.CloneVMIDBase) {
		errs = append(errs, fmt.Errorf("images.vmid_range %s overlaps the templates (%d-%d) or team clones (from %d)",
			c.Images.VMIDRange, c.Naming.TemplateVMIDBase, c.Naming.TemplateVMIDBase+99, c.Naming.CloneVMIDBase))
	}
	return errs
}

func overlapsRange(a, b, x, y int) bool { return a <= y && x <= b }

var subnetRE = regexp.MustCompile(`^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){2}$`)

// httpURL reports whether raw is an http(s) URL with a host and no path;
// the client adds /api2/json itself.
func httpURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// RequireTLSVerify reports an error unless proxmox.ca_file is set or
// insecure_skip_verify is explicitly on. Serve sends people's
// credentials to proxmox.url, and Proxmox's self-signed cert fails system CAs.
func (c Config) RequireTLSVerify() error {
	if c.Proxmox.CAFile == "" && !c.Proxmox.InsecureSkipVerify {
		return errors.New("proxmox.ca_file is required: the cluster CA that Proxmox's certificate is checked against " +
			"(copy it from any node: cat /etc/pve/pve-root-ca.pem; it is public). " +
			"Only to skip the check on purpose, set proxmox.insecure_skip_verify = true instead")
	}
	return nil
}

// RequireDatabase reports an error if no database is configured.
func (c Config) RequireDatabase() error {
	if strings.TrimSpace(c.Database.URL) == "" {
		return errors.New("database.url (or BATTLESHIP_DATABASE_URL) is required")
	}
	u, err := url.Parse(c.Database.URL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return errors.New("database.url must be a postgres:// URL with a host")
	}
	return nil
}
