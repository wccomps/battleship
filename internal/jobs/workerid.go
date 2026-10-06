package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// NewWorkerID returns a Worker.ID of the form prefix-hostname-pid-xxxxxxxx.
// The host and PID say where a job runs; the 8 random hex digits keep IDs
// unique even when several workers share a process, or a PID is reused on
// the same host (e.g. PID 1 in containers).
func NewWorkerID(prefix string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return fmt.Sprintf("%s-%s-%d-%s", prefix, host, os.Getpid(), hex.EncodeToString(b))
}
