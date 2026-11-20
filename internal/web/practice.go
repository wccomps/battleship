package web

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

// The session interface: what the practice app calls to deploy and tear down
// the session pods. PracticeHandler serves it on practice.listen; it shares
// no route with the volunteer pages (Handler).
//
// Every call carries practice.key and a Proxmox ticket of practice.principal.
// Outside a job, Battleship holds neither a ticket nor a password of that
// user: it proves each presented ticket with Proxmox and acts with it, as it
// does with a volunteer's. The interface's own checks stand in for a
// volunteer's preview and typed confirmation: pods inside practice.pods,
// deploy or teardown only, a deploy of an approved set only, and only its own
// jobs.

const (
	// practiceTicketHeader carries the principal's Proxmox ticket.
	practiceTicketHeader = "X-Proxmox-Ticket"
	// practiceMaxBody caps a request body: a job request is some 100 bytes,
	// and each template digest a deploy sends back about 100 more.
	practiceMaxBody = 16 << 10
	// practiceSetPlans caps the sets GET /v1/sets plans at once, and
	// practiceSetTimeout each plan, so a node that hangs fails the sets that
	// wait on it. Sets the call runs out of time for carry that error; the
	// listener's write timeout leaves the answer time to go out.
	practiceSetPlans   = 4
	practiceSetTimeout = 15 * time.Second
)

// PracticeCallTimeout bounds one call of the session interface. The
// listener's write timeout must be longer, so a call that runs out of time
// still answers.
const PracticeCallTimeout = 45 * time.Second

// PracticeHandler serves the session interface, or is nil when [practice] is
// unset. It believes no forwarding headers: the practice app connects
// directly.
func (s *Server) PracticeHandler() http.Handler {
	if !s.cfg.Practice.Enabled() {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/sets", s.practice(s.practiceSets))
	mux.Handle("GET /v1/pods/{pod}/vms", s.practice(s.practicePodVMs))
	mux.Handle("POST /v1/jobs", s.practice(s.practiceSubmit))
	mux.Handle("GET /v1/jobs/{id}", s.practice(s.practiceJob))
	mux.Handle("POST /v1/jobs/{id}/cancel", s.practice(s.practiceCancel))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such call")
	})
	return s.logRequests(mux)
}

// practiceCall is one call of the interface, made with the principal's
// proven ticket.
type practiceCall func(w http.ResponseWriter, r *http.Request, cred proxmox.Credential)

// practice guards a call: the key first, with no word to Proxmox, then the
// ticket, which Proxmox must accept as the principal's. The call gets the
// fresh ticket Proxmox answered with.
func (s *Server) practice(next practiceCall) http.Handler {
	principal := s.cfg.Practice.Principal
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(key))
		if subtle.ConstantTimeCompare(sum[:], s.practiceKey[:]) != 1 {
			practiceRefuse(w, http.StatusUnauthorized, "bad_key", "the key is missing or wrong")
			return
		}
		ticket := r.Header.Get(practiceTicketHeader)
		if ticket == "" {
			practiceRefuse(w, http.StatusUnauthorized, "no_ticket", "send "+principal+"'s Proxmox ticket in "+practiceTicketHeader)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), PracticeCallTimeout)
		defer cancel()
		// Renewing as the principal proves the ticket: Proxmox takes a ticket
		// as the password of the user it was issued to, and of no other.
		now := s.now()
		cred, err := s.tickets.RenewTicket(ctx, proxmox.Credential{User: principal, Ticket: ticket, LoginAt: now}, now)
		switch {
		case proxmox.RenewalRefused(err), err == nil && cred.User != principal:
			practiceRefuse(w, http.StatusForbidden, "ticket_refused", "Proxmox does not accept the ticket as "+principal+"'s")
			return
		case err != nil:
			s.practiceProxmoxFailed(w, "asking Proxmox about the ticket", err)
			return
		}
		next(w, r.WithContext(ctx), cred)
	})
}

type practiceProblem struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// Blocked names what the refused job could not do.
	Blocked []practiceBlocked `json:"blocked,omitempty"`
}

type practiceBlocked struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

func practiceAnswer(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func practiceRefuse(w http.ResponseWriter, status int, code, message string) {
	practiceAnswer(w, status, practiceProblem{Error: code, Message: message})
}

// practiceProxmoxFailed answers a call Proxmox failed: it answered with an
// error, or not at all.
func (s *Server) practiceProxmoxFailed(w http.ResponseWriter, what string, err error) {
	s.practiceFailed(w, what, err, http.StatusBadGateway, "proxmox_failed", what+": "+proxmox.Describe(err))
}

func (s *Server) practiceStoreFailed(w http.ResponseWriter, what string, err error) {
	s.practiceFailed(w, what, err, http.StatusInternalServerError, "failed", "battleship had a problem "+what)
}

// practiceFailed answers a call that failed through no fault of the request,
// as given unless it ran out of time.
func (s *Server) practiceFailed(w http.ResponseWriter, what string, err error, status int, code, message string) {
	s.logf("practice: %s: %v", what, err)
	if errors.Is(err, context.DeadlineExceeded) {
		status, code, message = http.StatusGatewayTimeout, "timed_out", what+" took too long"
	}
	practiceRefuse(w, status, code, message)
}

// practicePods reads a pod spec ("51", "51-53") whose pods all lie inside
// practice.pods. ok is false once it has refused the call.
func (s *Server) practicePods(w http.ResponseWriter, spec string) (teams []string, ok bool) {
	teams, err := pods.ParseTeams(spec)
	if err != nil {
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_pods", err.Error())
		return nil, false
	}
	lo, hi, _ := config.ParseRange(s.cfg.Practice.Pods) // validated at startup
	for _, t := range teams {
		if n, _ := strconv.Atoi(t); n < lo || n > hi {
			practiceRefuse(w, http.StatusForbidden, "pod_out_of_range", "the session interface acts on pods "+s.cfg.Practice.Pods+" only")
			return nil, false
		}
	}
	return teams, true
}

type practiceSet struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
	// Templates are the set's built templates the planner would use and the
	// principal may clone into the first session pod.
	Templates []practiceTemplate `json:"templates"`
	// Blocked are the templates the planner refuses, whose deploy to the
	// first session pod lacks one of the principal's rights, or which it may
	// not clone into that pod (pods.Uncloneable), and why.
	Blocked []practiceBlocked `json:"blocked"`
	// Pod is what one pod of the set takes: its clones of Templates
	// (pods.PodFootprint).
	Pod practiceSize `json:"pod"`
	// Error is why the set could not be planned; nothing else is then listed.
	Error string `json:"error,omitempty"`
}

type practiceTemplate struct {
	Host   string `json:"host"`
	Name   string `json:"name"`
	VMID   int    `json:"vmid"`
	Digest string `json:"digest"` // pods.TemplateSpec.Digest
}

type practiceSize struct {
	MemoryBytes int64 `json:"memory_bytes"`
	CPUs        int   `json:"cpus"`
	// BootDiskBytes is the boot disks a pod's full clones copy, the one size
	// Proxmox lists for a VM; it is 0 under linked clones (deploy.linked),
	// which share their template's, as pods.EstimateCapacity counts them.
	BootDiskBytes int64 `json:"boot_disk_bytes"`
}

// practiceSets lists the approved sets (pods.ApprovedSets) as the principal
// sees the cluster, each as a deploy of it is planned.
func (s *Server) practiceSets(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	ctx, api := r.Context(), s.as(cred)
	planner := pods.NewPlanner(api, s.cfg)
	vms, err := api.ClusterVMs(ctx)
	if err != nil {
		s.practiceProxmoxFailed(w, "listing the cluster's VMs", err)
		return
	}
	tags, err := planner.Access.TagAccess(ctx)
	if err != nil {
		s.practiceProxmoxFailed(w, "reading Proxmox's tag settings", err)
		return
	}
	approved, err := pods.ApprovedSets(vms, s.naming, s.cfg.Deploy, tags)
	if err != nil {
		practiceRefuse(w, http.StatusConflict, "not_approved", err.Error())
		return
	}
	listed := map[int]proxmox.VM{}
	for _, vm := range vms {
		listed[vm.VMID] = vm
	}
	lo, _, _ := config.ParseRange(s.cfg.Practice.Pods) // validated at startup
	sets := make([]practiceSet, len(approved))
	var g errgroup.Group
	g.SetLimit(practiceSetPlans)
	for i, set := range approved {
		g.Go(func() error {
			sets[i] = s.practiceSet(ctx, planner, set, pods.FormatTeam(lo), listed)
			return nil
		})
	}
	_ = g.Wait()
	practiceAnswer(w, http.StatusOK, struct {
		Sets []practiceSet `json:"sets"`
	}{sets})
}

// practiceSet describes one approved set as practiceDeploy of it to pod is
// planned, so the listing and a submission ask the same plan.
func (s *Server) practiceSet(ctx context.Context, planner pods.Planner, set pods.Set, pod string, listed map[int]proxmox.VM) practiceSet {
	empty := practiceSet{Name: string(set), Hosts: []string{}, Templates: []practiceTemplate{}, Blocked: []practiceBlocked{}}
	out := empty
	failed := func(err error) practiceSet {
		s.logf("practice: planning set %s: %v", set, err)
		empty.Error = proxmox.Describe(err)
		return empty
	}
	ctx, cancel := context.WithTimeout(ctx, practiceSetTimeout)
	defer cancel()
	plan, err := jobs.BuildPlan(ctx, planner, practiceDeploy(set, pod, nil))
	if err != nil {
		return failed(err)
	}
	unpermitted := map[string]string{} // by template: a right its deploy to pod lacks
	for _, it := range plan.Items {
		if it.Unpermitted {
			unpermitted[it.Template] = it.Blocked
		}
	}
	var cloned []pods.TemplateSpec
	for _, t := range plan.Templates {
		if !slices.Contains(out.Hosts, t.Host) {
			out.Hosts = append(out.Hosts, t.Host)
		}
		why := cmp.Or(t.Blocked, unpermitted[t.Name])
		if why == "" {
			// A pod that already holds the set clones nothing, so its plan
			// asks no clone right; it is asked here.
			if why, err = pods.Uncloneable(ctx, planner.Access, s.cfg, t, pod); err != nil {
				return failed(err)
			}
		}
		if why != "" {
			out.Blocked = append(out.Blocked, practiceBlocked{Name: t.Name, Reason: why})
			continue
		}
		out.Templates = append(out.Templates, practiceTemplate{Host: t.Host, Name: t.Name, VMID: t.VMID, Digest: t.Digest})
		cloned = append(cloned, t)
	}
	fp := pods.PodFootprint(cloned, listed, s.cfg.Deploy)
	out.Pod = practiceSize{MemoryBytes: fp.MemoryBytes, CPUs: fp.CPUs, BootDiskBytes: fp.DiskBytes}
	return out
}

type practiceVM struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Status string `json:"status"`
}

func (s *Server) practicePodVMs(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	teams, ok := s.practicePods(w, r.PathValue("pod"))
	if !ok {
		return
	}
	if len(teams) != 1 {
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_pods", "name one pod by its number, e.g. 51")
		return
	}
	all, err := s.as(cred).ClusterVMs(r.Context())
	if err != nil {
		s.practiceProxmoxFailed(w, "listing the cluster's VMs", err)
		return
	}
	vms := []practiceVM{}
	for _, vm := range pods.AllTeamVMs(all, s.naming) {
		if vm.Team == teams[0] {
			vms = append(vms, practiceVM{Name: vm.Name, Host: vm.Host, VMID: vm.VMID, Node: vm.Node, Status: vm.Status})
		}
	}
	practiceAnswer(w, http.StatusOK, struct {
		Pod string       `json:"pod"`
		VMs []practiceVM `json:"vms"`
	}{teams[0], vms})
}

// practiceJobRequest asks for a deploy of Set to Pods, or a teardown of
// everything in Pods. A deploy sends back Digests, the template digests GET
// /v1/sets listed for the set, by template name.
type practiceJobRequest struct {
	Kind    string            `json:"kind"`
	Pods    string            `json:"pods"`
	Set     string            `json:"set"`
	Digests map[string]string `json:"digests"`
}

// practiceDeploy is the deploy of set that the interface submits, with the
// digests it was sent, and plans with digests nil to list a set.
func practiceDeploy(set pods.Set, teams string, digests map[string]string) jobs.Inputs {
	return jobs.Inputs{Kind: pods.KindDeploy, Teams: teams, Pattern: set.Pattern(), ApprovedOnly: true, Digests: digests}.Normalize()
}

// practiceInputs is the job the request asks for. ok is false once it has
// refused the call. It contacts nothing.
func (s *Server) practiceInputs(w http.ResponseWriter, req practiceJobRequest) (in jobs.Inputs, ok bool) {
	switch pods.Kind(req.Kind) {
	case pods.KindDeploy:
		set, err := pods.ParseSet(req.Set)
		if err != nil || set == "" {
			practiceRefuse(w, http.StatusUnprocessableEntity, "bad_set", "a deploy names one set, e.g. kilo.alpha")
			return in, false
		}
		in = practiceDeploy(set, req.Pods, req.Digests)
	case pods.KindTeardown:
		if req.Set != "" {
			practiceRefuse(w, http.StatusUnprocessableEntity, "bad_set", "a teardown takes everything in its pods; leave set out")
			return in, false
		}
		in = jobs.Inputs{Kind: pods.KindTeardown, Teams: req.Pods}.Normalize()
	default:
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_kind", "kind must be deploy or teardown")
		return in, false
	}
	if _, ok := s.practicePods(w, req.Pods); !ok {
		return in, false
	}
	switch {
	case in.Kind == pods.KindDeploy && len(req.Digests) == 0:
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_digests",
			`a deploy sends back the digest GET /v1/sets listed for each of the set's templates, as "digests": {"<template name>": "<digest>"}`)
		return in, false
	case in.Kind == pods.KindTeardown && req.Digests != nil:
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_digests", "a teardown takes no digests; leave digests out")
		return in, false
	}
	if err := in.Validate(); err != nil {
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_request", err.Error())
		return in, false
	}
	return in, true
}

// practiceSubmit stores a deploy or teardown job that runs with the renewed
// ticket. A deploy is all or nothing: one blocked VM refuses it.
func (s *Server) practiceSubmit(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	var req practiceJobRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, practiceMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		practiceRefuse(w, http.StatusBadRequest, "bad_json",
			`send {"kind": "deploy" or "teardown", "pods": "51-53", and for a deploy "set": "kilo.alpha", "digests": {"<template name>": "<digest>"}}`)
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		practiceRefuse(w, http.StatusBadRequest, "bad_json", "send one JSON object and nothing after it")
		return
	}
	in, ok := s.practiceInputs(w, req)
	if !ok {
		return
	}

	ctx := r.Context()
	reserved, err := s.st.ReservedKeys(ctx, 0)
	if err != nil {
		s.practiceStoreFailed(w, "reading what queued jobs will create", err)
		return
	}
	planner := pods.NewPlanner(s.as(cred), s.cfg)
	planner.Reserved = func(context.Context) (map[string]bool, error) { return reserved, nil }
	// Past practiceInputs, planning fails only on what it reads of Proxmox.
	plan, err := jobs.BuildPlan(ctx, planner, in)
	switch {
	case errors.Is(err, pods.ErrNotApproved):
		practiceRefuse(w, http.StatusConflict, "not_approved", err.Error())
		return
	case err != nil:
		s.practiceProxmoxFailed(w, "planning the job", err)
		return
	}
	blocked := practiceBlockedIn(plan)
	if in.Kind == pods.KindDeploy && len(blocked) > 0 {
		practiceAnswer(w, http.StatusConflict, practiceProblem{
			Error: "blocked", Message: "the deploy cannot run whole, so nothing was submitted", Blocked: blocked,
		})
		return
	}
	id, err := jobs.Submit(ctx, s.st, in, plan, jobs.Submitter{User: cred.User, Origin: store.OriginSession, Credential: cred, Seal: s.creds})
	switch {
	case errors.Is(err, jobs.ErrNothingToRun):
		practiceAnswer(w, http.StatusConflict, practiceProblem{
			Error: "nothing_to_run", Message: "no VM would be changed, so nothing was submitted", Blocked: blocked,
		})
		return
	case err != nil:
		s.practiceStoreFailed(w, "submitting the job", err)
		return
	}
	s.logf("practice: job %d submitted: as=%q kind=%s pods=%s", id, cred.User, in.Kind, in.Teams)
	practiceAnswer(w, http.StatusCreated, struct {
		ID      int64             `json:"id"`
		Blocked []practiceBlocked `json:"blocked"`
	}{id, blocked})
}

func practiceBlockedIn(plan *pods.Plan) []practiceBlocked {
	blocked := []practiceBlocked{}
	for _, t := range plan.Templates {
		if t.Blocked != "" {
			blocked = append(blocked, practiceBlocked{Name: t.Name, Reason: t.Blocked})
		}
	}
	for _, it := range plan.Items {
		if it.Blocked != "" {
			blocked = append(blocked, practiceBlocked{Name: it.Name, Reason: it.Blocked})
		}
	}
	return blocked
}

// practiceOwned reads a job the interface submitted (store.OriginSession);
// any other job, and none, are the same 404.
func (s *Server) practiceOwned(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such job")
		return store.Job{}, false
	}
	j, err := s.st.Job(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && j.Origin != store.OriginSession:
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such job")
		return store.Job{}, false
	case err != nil:
		s.practiceStoreFailed(w, "reading the job", err)
		return store.Job{}, false
	}
	return j, true
}

type practiceItem struct {
	Name   string `json:"name"`
	Pod    string `json:"pod"`
	VMID   int    `json:"vmid"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type practiceJob struct {
	ID              int64          `json:"id"`
	Kind            string         `json:"kind"`
	Pods            string         `json:"pods"`
	Status          string         `json:"status"`
	Error           string         `json:"error"`
	CancelRequested bool           `json:"cancel_requested"`
	CreatedAt       time.Time      `json:"created_at"`
	StartedAt       *time.Time     `json:"started_at"`
	FinishedAt      *time.Time     `json:"finished_at"`
	Items           []practiceItem `json:"items"`
}

func (s *Server) practiceJob(w http.ResponseWriter, r *http.Request, _ proxmox.Credential) {
	j, ok := s.practiceOwned(w, r)
	if !ok {
		return
	}
	rows, err := s.st.Items(r.Context(), j.ID)
	if err != nil {
		s.practiceStoreFailed(w, "reading the job's VMs", err)
		return
	}
	items := make([]practiceItem, len(rows))
	for i, it := range rows {
		items[i] = practiceItem{Name: it.Name, Pod: it.Team, VMID: it.VMID, Status: it.Status, Error: it.Error}
	}
	practiceAnswer(w, http.StatusOK, practiceJob{
		ID: j.ID, Kind: j.Kind, Pods: readInputs(j).Teams, Status: j.Status, Error: j.Error,
		CancelRequested: j.CancelRequested,
		CreatedAt:       j.CreatedAt.UTC(), StartedAt: utcPtr(j.StartedAt), FinishedAt: utcPtr(j.FinishedAt),
		Items: items,
	})
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (s *Server) practiceCancel(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	j, ok := s.practiceOwned(w, r)
	if !ok {
		return
	}
	switch err := s.st.RequestCancel(r.Context(), j.ID, cred.User); {
	case errors.Is(err, store.ErrNotActive):
		practiceRefuse(w, http.StatusConflict, "not_active", "the job already finished")
	case errors.Is(err, store.ErrNotFound):
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such job")
	case err != nil:
		s.practiceStoreFailed(w, "cancelling the job", err)
	default:
		s.logf("practice: job %d cancel requested", j.ID)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
