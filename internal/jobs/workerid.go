package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// NewWorkerID returns prefix-hostname-pid-xxxxxxxx. The random suffix keeps
// IDs unique across workers in one process and reused PIDs (PID 1 in
// containers).
func NewWorkerID(prefix string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return fmt.Sprintf("%s-%s-%d-%s", prefix, host, os.Getpid(), hex.EncodeToString(b))
}
