package jobs

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestNewWorkerID(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("serve-%s-%d-", host, os.Getpid())
	suffix := regexp.MustCompile(`^[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for range 100 {
		id := NewWorkerID("serve")
		rest, ok := strings.CutPrefix(id, prefix)
		if !ok || !suffix.MatchString(rest) {
			t.Fatalf("NewWorkerID = %q, want %s<8 hex digits>", id, prefix)
		}
		if seen[id] {
			t.Fatalf("NewWorkerID repeated %q", id)
		}
		seen[id] = true
	}
}
