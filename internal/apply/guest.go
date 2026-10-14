package apply

import (
	"context"
	"errors"

	"github.com/wccomps/battleship/internal/pods"
)

var errGuestPending = errors.New("not implemented")

func (e *Executor) waitAgent(ctx context.Context, it *pods.Item) (bool, error) {
	return false, errGuestPending
}

func (e *Executor) configureGuest(ctx context.Context, it *pods.Item) (bool, error) {
	return false, errGuestPending
}

func (e *Executor) verifyGuest(ctx context.Context, it *pods.Item) (bool, error) {
	return false, errGuestPending
}
