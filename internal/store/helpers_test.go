package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wccomps/battleship/internal/store"
)

var ctx = context.Background()

func newJob(keys ...string) store.NewJob {
	return store.NewJob{
		Kind:        "teardown",
		Inputs:      json.RawMessage(`{"teams":"1"}`),
		Plan:        json.RawMessage(`{"kind":"teardown"}`),
		Fingerprint: "fp",
		LockKeys:    keys,
		CreatedBy:   "alice",
		Items: []store.NewItem{
			{Name: "team01-teak", Team: "01", VMID: 10121},
			{Name: "team01-dc", Team: "01", VMID: 10105, Blocked: "no snapshot"},
		},
	}
}

func create(t *testing.T, s *store.Store, keys ...string) int64 {
	t.Helper()
	id, err := s.CreateJob(ctx, newJob(keys...))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
