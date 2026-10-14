package proxmox

import (
	"context"
	"reflect"
	"testing"
)

func TestAgentExecSendsCommandListAndInput(t *testing.T) {
	f, c := newFake(t)
	f.routes["POST /nodes/cedar/qemu/2501/agent/exec"] = jsonData(`{"pid":77}`)
	pid, err := c.AgentExec(context.Background(), "cedar", 2501, []string{"/bin/sh", "-s"}, "echo hi\n")
	if err != nil || pid != 77 {
		t.Fatalf("AgentExec = %d, %v", pid, err)
	}
	want := "command=%2Fbin%2Fsh&command=-s&input-data=echo+hi%0A"
	if got := f.bodies["POST /nodes/cedar/qemu/2501/agent/exec"]; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestAgentExecStatusReadsNumbersAndBools(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/cedar/qemu/2501/agent/exec-status"] = jsonData(`{"exited":1,"exitcode":3,"out-data":"o","err-data":"e"}`)
	st, err := c.AgentExecStatus(context.Background(), "cedar", 2501, 77)
	if err != nil || st != (ExecStatus{Exited: true, ExitCode: 3, Out: "o", Err: "e"}) {
		t.Fatalf("status = %+v, %v", st, err)
	}
	f.routes["GET /nodes/cedar/qemu/2501/agent/exec-status"] = jsonData(`{"exited":false}`)
	if st, _ := c.AgentExecStatus(context.Background(), "cedar", 2501, 77); st.Exited {
		t.Error("exited:false read as exited")
	}
}

func TestAgentInterfacesAndHostname(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/cedar/qemu/2501/agent/network-get-interfaces"] = jsonData(`{"result":[
		{"name":"lo","hardware-address":"00:00:00:00:00:00","ip-addresses":[{"ip-address":"127.0.0.1","prefix":8}]},
		{"name":"ens18","hardware-address":"BC:24:11:00:00:01","ip-addresses":[{"ip-address":"192.168.100.10","prefix":24}]}]}`)
	nics, err := c.AgentInterfaces(context.Background(), "cedar", 2501)
	if err != nil {
		t.Fatal(err)
	}
	want := []GuestNIC{{Name: "lo", MAC: "00:00:00:00:00:00", IPs: []string{"127.0.0.1/8"}},
		{Name: "ens18", MAC: "bc:24:11:00:00:01", IPs: []string{"192.168.100.10/24"}}}
	if !reflect.DeepEqual(nics, want) {
		t.Errorf("nics = %+v", nics)
	}
	f.routes["GET /nodes/cedar/qemu/2501/agent/get-host-name"] = jsonData(`{"result":{"host-name":"teak"}}`)
	if h, err := c.AgentHostname(context.Background(), "cedar", 2501); err != nil || h != "teak" {
		t.Errorf("hostname = %q, %v", h, err)
	}
}
