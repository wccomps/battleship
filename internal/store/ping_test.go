package store_test

import (
	"context"
	"testing"

	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

func TestPing(t *testing.T) {
	_, url := storetest.NewWithURL(t)
	ctx := context.Background()
	s, err := store.Open(ctx, url, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Errorf("Ping on an open store = %v, want nil", err)
	}
	s.Close()
	if err := s.Ping(ctx); err == nil {
		t.Error("Ping on a closed store = nil, want an error")
	}
}
