package status

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func createJob(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.CreateJob(ctx, store.NewJob{
		Kind: "power", Inputs: json.RawMessage(`{}`), Plan: json.RawMessage(`{"kind":"power"}`),
		Fingerprint: "fp", LockKeys: []string{"team:01"}, CreatedBy: "lead",
		Items: []store.NewItem{{Name: "team01-dc", Team: "01", VMID: 10101}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// awaitConnect waits for the hub's LISTEN to be up; 10s only guards a hang.
func awaitConnect(t *testing.T, connected <-chan struct{}) {
	t.Helper()
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub didn't connect within 10s")
	}
}

func TestHubWithPostgres(t *testing.T) {
	st, url := storetest.NewWithURL(t)
	connected := make(chan struct{}, 4)
	listen := func(ctx context.Context) (<-chan store.Notice, error) {
		ch, err := st.Notifications(ctx)
		if err == nil {
			connected <- struct{}{}
		}
		return ch, err
	}
	h := NewHub(listen, HubOptions{Logf: t.Logf})
	h.minB, h.maxB = 10*time.Millisecond, 50*time.Millisecond
	jobs, cancelJobs := h.SubscribeTopics(TopicJobs)
	defer cancelJobs()
	job1, cancelJob1 := h.SubscribeTopics(JobTopic(1))
	defer cancelJob1()
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(rctx); close(done) }()
	defer func() { stop(); <-done }()
	awaitConnect(t, connected)
	for name, ch := range map[string]<-chan Msg{TopicJobs: jobs, JobTopic(1): job1} {
		if m := recv(t, ch); !m.Resync {
			t.Errorf("%s on the first connection got %+v, want a resync", name, m)
		}
	}

	id := createJob(t, st)
	if id != 1 {
		t.Fatalf("first job has ID %d, want 1", id)
	}
	if m := recv(t, jobs); m != (Msg{Topic: TopicJobs}) {
		t.Errorf("jobs got %+v", m)
	}
	if m := recv(t, job1); m != (Msg{Topic: JobTopic(1)}) {
		t.Errorf("job:1 got %+v", m)
	}

	// Kill the hub's LISTEN connection from the server side.
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var killed int
	if err := conn.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'LISTEN %'`).Scan(&killed); err != nil {
		t.Fatal(err)
	}
	if killed != 1 {
		t.Fatalf("killed %d LISTEN connections, want the hub's one", killed)
	}
	awaitConnect(t, connected)
	for name, ch := range map[string]<-chan Msg{TopicJobs: jobs, JobTopic(1): job1} {
		if m := recv(t, ch); m != (Msg{Topic: name, Resync: true}) {
			t.Errorf("%s after the reconnect got %+v, want a resync", name, m)
		}
	}
	id = createJob(t, st)
	if m := recv(t, jobs); m.Resync {
		t.Errorf("jobs after the reconnect got %+v, want job %d's notice", m, id)
	}
}
