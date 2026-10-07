// Package config loads the battleship TOML configuration.
//
// Competition-specific values live here so a new season is a config change,
// not a code change. Patterns use {team} (two-digit team number) and {host}
// (template hostname) placeholders.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Proxmox     Proxmox     `toml:"proxmox"`
	Naming      Naming      `toml:"naming"`
	Network     Network     `toml:"network"`
	Deploy      Deploy      `toml:"deploy"`
	Teardown    Teardown    `toml:"teardown"`
	Concurrency Concurrency `toml:"concurrency"`
	Retry       Retry       `toml:"retry"`
	Database    Database    `toml:"database"`
	Jobs        Jobs        `toml:"jobs"`
	Web         Web         `toml:"web"`
	OIDC        OIDC        `toml:"oidc"`
}

type Proxmox struct {
	URL string `toml:"url"` // e.g. https://192.0.2.123:8006
	// URLs are several nodes of the cluster, instead of URL: battleship
	// uses one at a time and fails over to the next when it is down.
	// Exactly one of URL and URLs is set.
	URLs               []string `toml:"urls"`
	InsecureSkipVerify bool     `toml:"insecure_skip_verify"` // default false; serve and worker need ca_file or this set to true
	// CAFile is a PEM bundle of the CAs Proxmox's certificate is checked
	// against, e.g. the cluster's CA (/etc/pve/pve-root-ca.pem). Setting
	// it turns verification on; it can't be combined with
	// insecure_skip_verify = true.
	CAFile string `toml:"ca_file"`
	// RootCAs is CAFile's certificates, read by Load.
	RootCAs *x509.CertPool `toml:"-"`
	// Realm is the Proxmox OpenID realm users sign in to Proxmox through.
	Realm string `toml:"realm"`
	// TicketRenewAfter is how old a Proxmox ticket (a session's or a
	// job's) gets before battleship renews it. Tickets last 2h and can only
	// be renewed while valid.
	TicketRenewAfter time.Duration `toml:"ticket_renew_after"`
	// SDNZone is the SDN zone of the team bridges (network.ext_bridge and
	// int_bridge), whose vnets a deploy needs SDN.Use on.
	SDNZone string `toml:"sdn_zone"`
	// TicketMaxAge is how long a Proxmox login lasts through renewals.
	// Renewal never asks the identity provider, so group changes in
	// Authentik apply only at the next login: after this, a session signs
	// in to Proxmox again, and a job's credential lapses.
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
	// BaselinePatterns are globs (path.Match) naming other snapshots that
	// count as a VM's baseline when it lacks SnapshotName, e.g. the
	// fresh_clone_<timestamp> snapshots of the old deploy tool. A reset that
	// names no snapshot rolls back to SnapshotName, else the newest match.
	BaselinePatterns []string `toml:"baseline_patterns"`
	GPUVGA           string   `toml:"gpu_vga"` // templates with this vga value are pinned to their node
}

// Concurrency limits Proxmox work. Workers applies to each job on its own.
// ClonesPerNode, ConfigCalls and TemplateBuilds are shared by every job in
// one process, and each process has its own. With a database, Deletes and
// the storage operations that run one at a time are capped across every
// process that uses it; processes whose Deletes differ are capped at the
// largest of them.
type Concurrency struct {
	Workers       int `toml:"workers"`         // VMs processed at once
	ClonesPerNode int `toml:"clones_per_node"` // concurrent clone tasks per source node
	ConfigCalls   int `toml:"config_calls"`    // concurrent API calls from jobs and the drift scan, clone POSTs included
	// TemplateBuilds caps the templates built at once. Each build also takes
	// one of its master node's clone slots for its copy.
	TemplateBuilds int `toml:"template_builds"`
	// Deletes caps the VM destroy tasks running at once. Each destroy
	// removes the VM from its pool and ACLs under Proxmox's cluster-wide
	// lock on user.cfg; too many at once time out on that lock and leave
	// VMs half-deleted (locked as destroyed).
	Deletes int `toml:"deletes"`
}

// Teardown configures how team VMs are taken down before they are deleted.
type Teardown struct {
	// ShutdownTimeout is how long a running VM gets to shut down cleanly
	// before Proxmox hard-stops it. A clean shutdown lets a router
	// release its DHCP lease (udhcpc -R), so the next deploy gets the
	// address at once.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`
}

type Retry struct {
	TransientPatterns []string      `toml:"transient_patterns"` // case-insensitive substrings
	Attempts          int           `toml:"attempts"`           // tries per API call; also the most times a task that failed transiently is started
	InitialBackoff    time.Duration `toml:"initial_backoff"`
	MaxBackoff        time.Duration `toml:"max_backoff"`
	Rounds            int           `toml:"rounds"`      // end-of-job retry rounds
	RoundPause        time.Duration `toml:"round_pause"` // pause before each round
	TaskPoll          time.Duration `toml:"task_poll"`   // Proxmox task status poll interval
}

// Database is where jobs are stored. The worker, the web app and -queue need
// it; when it is set, direct CLI runs go through it too.
type Database struct {
	URL string `toml:"url"` // postgres://...; overridden by BATTLESHIP_DATABASE_URL
	// MaxConns caps the process's main connection pool (at least 4,
	// default 16). Each process also opens up to 2 more for job
	// heartbeats and readiness checks, 1 for its delete and storage slots
	// and 1 that LISTENs for job changes, so it uses up to MaxConns+4;
	// budget that times the processes against Postgres's max_connections.
	MaxConns int `toml:"max_conns"`
	// SealKey encrypts the Proxmox credentials jobs keep in the database
	// (their submitters' tickets or tokens). Every process that submits or
	// runs jobs needs the same key. Overridden by BATTLESHIP_SEAL_KEY.
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

// RequireSealKey reports an error if no seal key is configured. Commands
// that submit or run jobs call it.
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
	Poll       time.Duration `toml:"poll"`        // how often an idle worker looks for jobs
	// CancelGrace is how long, after a cancel, a step that already sent
	// Proxmox a change may wait for that change's task to end, so the job
	// records what it did (apply.ErrCancelRequested).
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
		Concurrency: Concurrency{Workers: 20, ClonesPerNode: 10, ConfigCalls: 5, TemplateBuilds: 4, Deletes: 4},
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
			Attempts:       6,
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

// Endpoints is the Proxmox API URLs in the order to try them: URLs, or
// URL alone.
func (p Proxmox) Endpoints() []string {
	if len(p.URLs) > 0 {
		return p.URLs
	}
	if p.URL != "" {
		return []string{p.URL}
	}
	return nil
}

// ReadCAFile reads a PEM bundle into a pool, failing unless it holds at
// least one certificate.
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

// removedKeys are settings battleship no longer has, with why. A config that
// still sets them fails to load, naming them, so an old config can't
// silently keep a service token, or a team list nothing reads.
var removedKeys = map[string]string{
	"proxmox.token_id":     removedToken,
	"proxmox.token_secret": removedToken,
	"roles.operators":      removedToken,
	"roles.leads":          removedToken,
	"web.teams":            "Battleship shows the teams that have VMs; delete the line",
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

	// Naming: {team} patterns (in fixed order for deterministic errors)
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

	if !strings.Contains(c.Naming.VMName, "{host}") {
		errs = append(errs, errors.New("naming.vm_name must contain {host}"))
	}

	if c.Naming.TemplateSuffix == "" {
		errs = append(errs, errors.New("naming.template_suffix is required"))
	}

	// Naming: stride must be at least 100
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
	if c.Concurrency.ClonesPerNode < 1 {
		errs = append(errs, errors.New("concurrency.clones_per_node must be at least 1"))
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

	// A worker stops its job after StaleAfter/2 without a heartbeat: 5
	// beats per StaleAfter survive one failed beat, 4 would not.
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

	errs = append(errs, c.validateWeb()...)
	return errors.Join(errs...)
}

// httpURL reports whether raw is an http or https URL with a host and no
// path beyond "/": the client adds /api2/json itself.
func httpURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

// RequireTLSVerify reports an error unless Proxmox's certificate will be
// checked against the cluster CA (proxmox.ca_file), or checking was turned
// off on purpose (insecure_skip_verify = true). People's tickets and tokens
// go to whatever answers at proxmox.url, so battleship serve and battleship
// worker need one of the two; Proxmox's self-signed certificate never
// passes the system's CAs.
func (c Config) RequireTLSVerify() error {
	if c.Proxmox.CAFile == "" && !c.Proxmox.InsecureSkipVerify {
		return errors.New("proxmox.ca_file is required: the cluster CA that Proxmox's certificate is checked against " +
			"(copy it from any node: cat /etc/pve/pve-root-ca.pem; it is public). " +
			"Only to skip the check on purpose, set proxmox.insecure_skip_verify = true instead")
	}
	return nil
}

// RequireDatabase reports an error if no database is configured. Commands
// that store jobs call it; the plain CLI doesn't need a database.
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
