package main

import (
	"testing"
)

// serve warns when told not to check Proxmox's certificate.
func TestServeWarnsWhenProxmoxIsUnchecked(t *testing.T) {
	e := startServe(t)
	if recs := e.records("Proxmox's certificate is not checked"); len(recs) != 1 || recs[0]["level"] != "WARN" {
		t.Fatalf("records = %v", recs)
	}
}
