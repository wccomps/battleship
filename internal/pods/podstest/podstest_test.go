package podstest_test

import (
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/pods/podstest"
)

var _ pods.API = (*podstest.Fake)(nil)
