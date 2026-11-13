package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

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
// Battleship holds neither a ticket nor a password of that user: it proves
// each presented ticket with Proxmox and acts with it, as it does with a
// volunteer's. The interface's own checks stand in for a volunteer's preview
// and typed confirmation: pods inside practice.pods, deploy or teardown only,
// a deploy of an approved set only, and only its own jobs.

const (
	// practiceActor is who the interface's jobs are recorded as created by,
	// for the job log. It is a label, not an identity: the interface tells
	// its jobs by the user they act as (practiceOwned). The space keeps it
	// from being anyone's email address, which mayCancel compares.
	practiceActor = "session interface"
	// practiceTicketHeader carries the principal's Proxmox ticket.
	practiceTicketHeader = "X-Proxmox-Ticket"
	// practiceMaxBody caps a request body; a job request is under 100 bytes.
	practiceMaxBody = 4 << 10
)

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
		// Renewing as the principal proves the ticket: Proxmox takes a ticket
		// as the password of the user it was issued to, and of no other.
		now := s.now()
		cred, err := s.tickets.RenewTicket(r.Context(), proxmox.Credential{User: principal, Ticket: ticket, LoginAt: now}, now)
		switch {
		case proxmox.RenewalRefused(err), err == nil && cred.User != principal:
			practiceRefuse(w, http.StatusForbidden, "ticket_refused", "Proxmox does not accept the ticket as "+principal+"'s")
			return
		case err != nil:
			s.logf("practice: proving the ticket: %v", err)
			practiceRefuse(w, http.StatusBadGateway, "proxmox_failed", "Proxmox could not be asked about the ticket: "+proxmox.Describe(err))
			return
		}
		next(w, r, cred)
	})
}

type practiceProblem struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// Blocked names what a refused deploy could not do.
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

// practiceFailed answers a failed read of Proxmox or the database.
func (s *Server) practiceFailed(w http.ResponseWriter, what string, err error) {
	s.logf("practice: %s: %v", what, err)
	if fromProxmox(err) {
		practiceRefuse(w, http.StatusBadGateway, "proxmox_failed", what+": "+proxmox.Describe(err))
		return
	}
	practiceRefuse(w, http.StatusInternalServerError, "failed", "battleship had a problem "+what)
}

// fromProxmox reports whether err is Proxmox's answer rather than a problem
// of battleship's own.
func fromProxmox(err error) bool {
	var api *proxmox.APIError
	return errors.As(err, &api)
}

// practicePods reads a pod spec ("51", "51-53") and reports whether every pod
// lies inside practice.pods.
func (s *Server) practicePods(spec string) (teams []string, inRange bool, err error) {
	if teams, err = pods.ParseTeams(spec); err != nil {
		return nil, false, err
	}
	lo, hi := s.cfg.Practice.PodRange()
	for _, t := range teams {
		if n, _ := strconv.Atoi(t); n < lo || n > hi {
			return teams, false, nil
		}
	}
	return teams, true, nil
}

func (s *Server) practiceOutOfRange(w http.ResponseWriter) {
	practiceRefuse(w, http.StatusForbidden, "pod_out_of_range", "the session interface acts on pods "+s.cfg.Practice.Pods+" only")
}

type practiceSet struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
	// Templates are the templates pods would be cloned from, one per host
	// that has one built.
	Templates []practiceTemplate `json:"templates"`
	// Pod is what one pod of the set takes: the sum over its hosts, each
	// sized by its template, or by its master where none is built.
	Pod practiceSize `json:"pod"`
}

type practiceTemplate struct {
	Host string `json:"host"`
	Name string `json:"name"`
	VMID int    `json:"vmid"`
	// Digest is Proxmox's digest of the template's config. A rebuilt
	// template is a new clone of its master, with a config of its own.
	Digest string `json:"digest"`
	node   string
}

type practiceSize struct {
	MemoryBytes int64 `json:"memory_bytes"`
	CPUs        int   `json:"cpus"`
	DiskBytes   int64 `json:"disk_bytes"`
}

// practiceConfigReads caps the template configs read at once for one call.
const practiceConfigReads = 8

// practiceSets lists the sets whose masters, as the principal sees them, all
// carry deploy.approved_tag, each with its templates and the size of a pod.
func (s *Server) practiceSets(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	api := s.as(cred)
	vms, err := api.ClusterVMs(r.Context())
	if err != nil {
		s.practiceFailed(w, "listing the cluster's VMs", err)
		return
	}
	all, _ := pods.MasterSets(vms, s.naming, s.cfg.Deploy.MasterTag, s.cfg.Deploy.ApprovedTag)
	sets := []practiceSet{}
	for _, set := range all {
		if !set.AllApproved() {
			continue
		}
		ps := practiceSet{Name: string(set.Name), Hosts: set.Hosts, Templates: []practiceTemplate{}}
		for _, m := range pods.SetMembers(vms, s.naming, set.Name, s.cfg.Deploy.MasterTag) {
			sized := m.Master
			if m.Template != nil {
				sized = *m.Template
				ps.Templates = append(ps.Templates, practiceTemplate{Host: m.Host, Name: sized.Name, VMID: sized.VMID, node: sized.Node})
			}
			ps.Pod.MemoryBytes += sized.MaxMem
			ps.Pod.CPUs += sized.MaxCPU
			ps.Pod.DiskBytes += sized.MaxDisk
		}
		sets = append(sets, ps)
	}
	g, ctx := errgroup.WithContext(r.Context())
	g.SetLimit(practiceConfigReads)
	for i := range sets {
		for j := range sets[i].Templates {
			t := &sets[i].Templates[j]
			g.Go(func() error {
				cfg, err := api.VMConfig(ctx, t.node, t.VMID)
				if err != nil {
					return err
				}
				if t.Digest = cfg["digest"]; t.Digest == "" {
					return fmt.Errorf("Proxmox gave no digest for %s's config", t.Name)
				}
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		s.practiceFailed(w, "reading the templates' configs", err)
		return
	}
	practiceAnswer(w, http.StatusOK, struct {
		Sets []practiceSet `json:"sets"`
	}{sets})
}

type practiceVM struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Status string `json:"status"`
}

// practicePodVMs lists one session pod's VMs.
func (s *Server) practicePodVMs(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	teams, inRange, err := s.practicePods(r.PathValue("pod"))
	switch {
	case err != nil || len(teams) != 1:
		practiceRefuse(w, http.StatusUnprocessableEntity, "bad_pod", "name one pod by its number, e.g. 51")
		return
	case !inRange:
		s.practiceOutOfRange(w)
		return
	}
	all, err := s.as(cred).ClusterVMs(r.Context())
	if err != nil {
		s.practiceFailed(w, "listing the cluster's VMs", err)
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
// everything in Pods.
type practiceJobRequest struct {
	Kind string `json:"kind"`
	Pods string `json:"pods"`
	Set  string `json:"set"`
}

// inputs is the job the request asks for, or why it is refused. It contacts
// nothing.
func (s *Server) practiceInputs(req practiceJobRequest) (in jobs.Inputs, status int, code, message string) {
	switch pods.Kind(req.Kind) {
	case pods.KindDeploy:
		set, err := pods.ParseSet(req.Set)
		if err != nil || set == "" {
			return in, http.StatusUnprocessableEntity, "bad_set", "a deploy names one set, e.g. kilo.alpha"
		}
		in = jobs.Inputs{Kind: pods.KindDeploy, Pattern: set.Pattern(), ApprovedOnly: true}
	case pods.KindTeardown:
		if req.Set != "" {
			return in, http.StatusUnprocessableEntity, "bad_set", "a teardown takes everything in its pods; leave set out"
		}
		in = jobs.Inputs{Kind: pods.KindTeardown}
	default:
		return in, http.StatusUnprocessableEntity, "bad_kind", "kind must be deploy or teardown"
	}
	_, inRange, err := s.practicePods(req.Pods)
	switch {
	case err != nil:
		return in, http.StatusUnprocessableEntity, "bad_pods", err.Error()
	case !inRange:
		return in, http.StatusForbidden, "pod_out_of_range", "the session interface acts on pods " + s.cfg.Practice.Pods + " only"
	}
	in.Teams = req.Pods
	in = in.Normalize()
	if err := in.Validate(); err != nil {
		return in, http.StatusUnprocessableEntity, "bad_request", err.Error()
	}
	return in, 0, "", ""
}

// practiceSubmit stores a deploy or teardown job that runs with the
// presented ticket. A deploy is all or nothing: one blocked VM refuses it.
func (s *Server) practiceSubmit(w http.ResponseWriter, r *http.Request, cred proxmox.Credential) {
	var req practiceJobRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, practiceMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		practiceRefuse(w, http.StatusBadRequest, "bad_json", `send {"kind": "deploy" or "teardown", "pods": "51-53", "set": "kilo.alpha" for a deploy}`)
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		practiceRefuse(w, http.StatusBadRequest, "bad_json", "send one JSON object and nothing after it")
		return
	}
	in, status, code, message := s.practiceInputs(req)
	if status != 0 {
		practiceRefuse(w, status, code, message)
		return
	}

	ctx := r.Context()
	planner := pods.NewPlanner(s.as(cred), s.cfg)
	planner.Reserved = func(ctx context.Context) (map[string]bool, error) { return s.st.ReservedKeys(ctx, 0) }
	plan, err := jobs.BuildPlan(ctx, planner, in)
	switch {
	case errors.Is(err, pods.ErrNotApproved):
		practiceRefuse(w, http.StatusConflict, "not_approved", err.Error())
		return
	case fromProxmox(err):
		s.practiceFailed(w, "planning the job", err)
		return
	case err != nil:
		practiceRefuse(w, http.StatusConflict, "plan_failed", proxmox.Describe(err))
		return
	}
	// "*.alpha" also matches kilo.alpha's masters. The request named one set.
	if set, _ := pods.SetOfPattern(in.Pattern); in.Kind == pods.KindDeploy {
		for _, t := range plan.Templates {
			if _, of := s.naming.SplitMaster(t.MasterName); of != set {
				practiceRefuse(w, http.StatusConflict, "plan_failed", fmt.Sprintf(
					"a deploy of %s would take in %s, of set %s: one set's name ends the other's, so it can't be deployed alone", set, t.MasterName, of))
				return
			}
		}
	}
	blocked := practiceBlockedIn(plan)
	if in.Kind == pods.KindDeploy && len(blocked) > 0 {
		practiceAnswer(w, http.StatusConflict, practiceProblem{
			Error: "blocked", Message: "the deploy cannot run whole, so nothing was submitted", Blocked: blocked,
		})
		return
	}
	id, err := jobs.Submit(ctx, s.st, in, plan, jobs.Submitter{User: practiceActor, Credential: cred, Seal: s.creds})
	switch {
	case errors.Is(err, jobs.ErrNothingToRun):
		practiceAnswer(w, http.StatusConflict, practiceProblem{
			Error: "nothing_to_run", Message: "no VM would be changed, so nothing was submitted", Blocked: blocked,
		})
		return
	case err != nil:
		s.practiceFailed(w, "submitting the job", err)
		return
	}
	s.logf("practice: job %d submitted: as=%q kind=%s pods=%s", id, cred.User, in.Kind, in.Teams)
	practiceAnswer(w, http.StatusCreated, struct {
		ID      int64             `json:"id"`
		Blocked []practiceBlocked `json:"blocked"`
	}{id, blocked})
}

// practiceBlockedIn is what plan would not do, and why.
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

// practiceOwned reads a job the interface created. Any other job, and none,
// are the same 404.
//
// A job is the interface's if it acts as the principal. Only the interface
// can make one: a volunteer's job acts as a user of proxmox.realm, and config
// refuses a principal in that realm.
func (s *Server) practiceOwned(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such job")
		return store.Job{}, false
	}
	j, err := s.st.Job(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && j.CreatedAs != s.cfg.Practice.Principal:
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such job")
		return store.Job{}, false
	case err != nil:
		s.practiceFailed(w, "reading the job", err)
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

// practiceJob reads one of the interface's jobs with its VMs.
func (s *Server) practiceJob(w http.ResponseWriter, r *http.Request, _ proxmox.Credential) {
	j, ok := s.practiceOwned(w, r)
	if !ok {
		return
	}
	rows, err := s.st.Items(r.Context(), j.ID)
	if err != nil {
		s.practiceFailed(w, "reading the job's VMs", err)
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

// practiceCancel asks one of the interface's jobs to stop.
func (s *Server) practiceCancel(w http.ResponseWriter, r *http.Request, _ proxmox.Credential) {
	j, ok := s.practiceOwned(w, r)
	if !ok {
		return
	}
	switch err := s.st.RequestCancel(r.Context(), j.ID, practiceActor); {
	case errors.Is(err, store.ErrNotActive):
		practiceRefuse(w, http.StatusConflict, "not_active", "the job already finished")
	case errors.Is(err, store.ErrNotFound):
		practiceRefuse(w, http.StatusNotFound, "not_found", "no such job")
	case err != nil:
		s.practiceFailed(w, "cancelling the job", err)
	default:
		s.logf("practice: job %d cancel requested", j.ID)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
