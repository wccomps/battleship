package store_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func newPreview(session, id string) store.Preview {
	return store.Preview{
		ID:          id,
		SessionID:   session,
		Kind:        "reset",
		Inputs:      json.RawMessage(`{"kind":"reset","teams":"1"}`),
		Fingerprint: "fp-1",
		CreatedAt:   t0,
		ExpiresAt:   t0.Add(30 * time.Minute),
	}
}

func createPreview(t *testing.T, s *store.Store, p store.Preview) {
	t.Helper()
	if err := s.CreatePreview(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewRoundTrip(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	want := newPreview("sess-a", "prev-1")
	createPreview(t, s, want)

	got, err := s.Preview(ctx, "sess-a", "prev-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("times = %v, %v; want %v, %v", got.CreatedAt, got.ExpiresAt, want.CreatedAt, want.ExpiresAt)
	}
	if got.ID != want.ID || got.SessionID != want.SessionID || got.Kind != want.Kind ||
		got.Fingerprint != want.Fingerprint || got.JobID != 0 {
		t.Errorf("preview = %+v, want %+v", got, want)
	}
	var in map[string]any
	if err := json.Unmarshal(got.Inputs, &in); err != nil || in["teams"] != "1" || in["kind"] != "reset" {
		t.Errorf("inputs = %s (%v)", got.Inputs, err)
	}
}

func TestPreviewBelongsToItsSession(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	createSession(t, s, newSession("sess-b"))
	createPreview(t, s, newPreview("sess-a", "prev-1"))

	if _, err := s.Preview(ctx, "sess-b", "prev-1"); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("another session's preview: err = %v, want ErrPreviewNotFound", err)
	}
	if _, err := s.Preview(ctx, "sess-a", "nope"); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("unknown preview: err = %v, want ErrPreviewNotFound", err)
	}
	// Submitting it from the other session doesn't work either.
	nj := newJob("team:01")
	nj.Preview = &store.PreviewClaim{SessionID: "sess-b", ID: "prev-1"}
	if _, err := s.CreateJob(ctx, nj); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("CreateJob with another session's preview: err = %v, want ErrPreviewNotFound", err)
	}
	if jobs, _ := s.Jobs(ctx, 10); len(jobs) != 0 {
		t.Errorf("jobs = %d, want none", len(jobs))
	}
}

func TestCreatePreviewNeedsItsFields(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	for name, mut := range map[string]func(*store.Preview){
		"id":          func(p *store.Preview) { p.ID = "" },
		"session":     func(p *store.Preview) { p.SessionID = "" },
		"kind":        func(p *store.Preview) { p.Kind = "" },
		"inputs":      func(p *store.Preview) { p.Inputs = nil },
		"fingerprint": func(p *store.Preview) { p.Fingerprint = "" },
		"expiry":      func(p *store.Preview) { p.ExpiresAt = time.Time{} },
	} {
		p := newPreview("sess-a", "prev-"+name)
		mut(&p)
		if err := s.CreatePreview(ctx, p); !errors.Is(err, store.ErrEmptyPreview) {
			t.Errorf("without %s: err = %v, want ErrEmptyPreview", name, err)
		}
	}
	// A preview needs a live session.
	if err := s.CreatePreview(ctx, newPreview("gone", "prev-x")); err == nil {
		t.Error("preview of an unknown session was stored")
	}
}

func TestCreatePreviewClearsTheSessionsExpiredOnes(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	createSession(t, s, newSession("sess-b"))
	old := newPreview("sess-a", "old")
	createPreview(t, s, old)
	otherOld := newPreview("sess-b", "other-old")
	createPreview(t, s, otherOld)

	later := newPreview("sess-a", "new")
	later.CreatedAt = old.ExpiresAt // the old one has just expired
	later.ExpiresAt = later.CreatedAt.Add(30 * time.Minute)
	createPreview(t, s, later)

	if _, err := s.Preview(ctx, "sess-a", "old"); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("expired preview: err = %v, want it deleted", err)
	}
	if _, err := s.Preview(ctx, "sess-a", "new"); err != nil {
		t.Errorf("new preview: %v", err)
	}
	// Other sessions' previews are theirs to clear.
	if _, err := s.Preview(ctx, "sess-b", "other-old"); err != nil {
		t.Errorf("another session's expired preview was deleted: %v", err)
	}
}

func TestPreviewsGoWithTheirSession(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	createPreview(t, s, newPreview("sess-a", "prev-1"))
	if err := s.DeleteSession(ctx, "sess-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview(ctx, "sess-a", "prev-1"); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("after logout: err = %v, want ErrPreviewNotFound", err)
	}
}

func TestDeleteExpiredPreviews(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	createSession(t, s, newSession("sess-b"))
	for id, exp := range map[string]time.Duration{"gone": -time.Minute, "now": 0, "soon": time.Second, "later": time.Hour} {
		p := newPreview("sess-a", id)
		p.CreatedAt = t0.Add(-time.Hour)
		p.ExpiresAt = t0.Add(exp)
		createPreview(t, s, p)
	}
	// A submitted preview that expired stays while its session does, so a
	// late repeat of its confirm still finds its job.
	used := newPreview("sess-b", "used")
	used.CreatedAt, used.ExpiresAt = t0.Add(-time.Hour), t0.Add(-time.Minute)
	createPreview(t, s, used)
	nj := newJob("team:01")
	nj.Preview = &store.PreviewClaim{SessionID: "sess-b", ID: "used"}
	jobID, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteExpiredPreviews(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("deleted %d previews, want 2 (unsubmitted, expired before or at now)", n)
	}
	for _, k := range []struct {
		session, id string
		live        bool
	}{{"sess-a", "gone", false}, {"sess-a", "now", false}, {"sess-a", "soon", true}, {"sess-a", "later", true}, {"sess-b", "used", true}} {
		_, err := s.Preview(ctx, k.session, k.id)
		if k.live && err != nil {
			t.Errorf("%s: %v, want kept", k.id, err)
		}
		if !k.live && !errors.Is(err, store.ErrPreviewNotFound) {
			t.Errorf("%s: err = %v, want deleted", k.id, err)
		}
	}
	if _, err := s.Job(ctx, jobID); err != nil {
		t.Errorf("the used preview's job: %v, want kept", err)
	}
	if _, err := s.Session(ctx, "sess-a"); err != nil {
		t.Errorf("session: %v, want kept", err)
	}
	if p, err := s.Preview(ctx, "sess-b", "used"); err != nil || p.JobID != jobID {
		t.Errorf("the used preview = %+v, %v; want kept with job %d", p, err, jobID)
	}

	// The submitted preview goes with its session; its job stays.
	if err := s.DeleteSession(ctx, "sess-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview(ctx, "sess-b", "used"); !errors.Is(err, store.ErrPreviewNotFound) {
		t.Errorf("the used preview after its session went: err = %v, want deleted", err)
	}
	if _, err := s.Job(ctx, jobID); err != nil {
		t.Errorf("the used preview's job after its session went: %v, want kept", err)
	}
}

func TestCreateJobConsumesPreviewOnce(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	createPreview(t, s, newPreview("sess-a", "prev-1"))

	nj := newJob("team:01")
	nj.Preview = &store.PreviewClaim{SessionID: "sess-a", ID: "prev-1"}
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(ctx, "sess-a", "prev-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.JobID != id {
		t.Errorf("preview's job = %d, want %d", p.JobID, id)
	}

	if _, err := s.CreateJob(ctx, nj); !errors.Is(err, store.ErrPreviewUsed) {
		t.Errorf("second submit: err = %v, want ErrPreviewUsed", err)
	}
	if jobs, _ := s.Jobs(ctx, 10); len(jobs) != 1 {
		t.Errorf("jobs = %d, want 1", len(jobs))
	}
}

// A double-clicked confirm sends two requests at once; exactly one job
// comes of it, whichever replica each lands on.
func TestConcurrentSubmitsOfOnePreviewMakeOneJob(t *testing.T) {
	s := storetest.New(t)
	createSession(t, s, newSession("sess-a"))
	createPreview(t, s, newPreview("sess-a", "prev-1"))

	const n = 8
	var wg sync.WaitGroup
	ids := make([]int64, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			nj := newJob("team:01")
			nj.Preview = &store.PreviewClaim{SessionID: "sess-a", ID: "prev-1"}
			ids[i], errs[i] = s.CreateJob(ctx, nj)
		})
	}
	close(start)
	wg.Wait()

	var made []int64
	for i := range n {
		switch {
		case errs[i] == nil:
			made = append(made, ids[i])
		case !errors.Is(errs[i], store.ErrPreviewUsed):
			t.Errorf("submit %d: %v, want nil or ErrPreviewUsed", i, errs[i])
		}
	}
	if len(made) != 1 {
		t.Fatalf("jobs made = %v, want exactly one", made)
	}
	jobs, err := s.Jobs(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ID != made[0] {
		t.Errorf("stored jobs = %+v (%v), want just job %d", jobs, err, made[0])
	}
	if p, _ := s.Preview(ctx, "sess-a", "prev-1"); p.JobID != made[0] {
		t.Errorf("preview's job = %d, want %d", p.JobID, made[0])
	}
}

func TestJobRecordsWhoItActsAs(t *testing.T) {
	s := storetest.New(t)
	nj := newJob("team:01")
	nj.CreatedAs = "alice@auth.example.org"
	id, err := s.CreateJob(ctx, nj)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.CreatedBy != "alice" || j.CreatedAs != "alice@auth.example.org" || j.CreatedRole != "" {
		t.Errorf("created by %q as %q (role %q)", j.CreatedBy, j.CreatedAs, j.CreatedRole)
	}
}
