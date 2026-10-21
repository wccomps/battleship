package proxmox

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"
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
	st, err := c.AgentExecStatus(context.Background(), "cedar", 2501, 77, execCap)
	if err != nil || st != (ExecStatus{Exited: true, ExitCode: 3, Out: "o", Err: "e"}) {
		t.Fatalf("status = %+v, %v", st, err)
	}
	f.routes["GET /nodes/cedar/qemu/2501/agent/exec-status"] = jsonData(`{"exited":false}`)
	if st, _ := c.AgentExecStatus(context.Background(), "cedar", 2501, 77, execCap); st.Exited {
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
	want := []GuestNIC{{Name: "lo", MAC: "00:00:00:00:00:00", IPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		{Name: "ens18", MAC: "bc:24:11:00:00:01", IPs: []netip.Prefix{netip.MustParsePrefix("192.168.100.10/24")}}}
	if !reflect.DeepEqual(nics, want) {
		t.Errorf("nics = %+v", nics)
	}
	f.routes["GET /nodes/cedar/qemu/2501/agent/get-host-name"] = jsonData(`{"result":{"host-name":"teak"}}`)
	if h, err := c.AgentHostname(context.Background(), "cedar", 2501); err != nil || h != "teak" {
		t.Errorf("hostname = %q, %v", h, err)
	}
}

// An IPv4 address written as IPv6 reads as IPv4, so every caller sees one
// form of it.
func TestAgentInterfacesReadIPv4WrittenAsIPv6(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/cedar/qemu/2501/agent/network-get-interfaces"] = jsonData(`{"result":[
		{"name":"ens18","hardware-address":"BC:24:11:00:00:01","ip-addresses":[
			{"ip-address":"::ffff:10.50.12.133","prefix":120},
			{"ip-address":"::FFFF:10.50.12.134","prefix":64},
			{"ip-address":"::ffff:10.50.12.135"}]}]}`)
	nics, err := c.AgentInterfaces(context.Background(), "cedar", 2501)
	if err != nil || len(nics) != 1 {
		t.Fatalf("nics = %+v, %v", nics, err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.50.12.133/24"),
		netip.PrefixFrom(netip.MustParseAddr("10.50.12.134"), -1), netip.PrefixFrom(netip.MustParseAddr("10.50.12.135"), -1)}
	if !slices.Equal(nics[0].IPs, want) {
		t.Errorf("addresses %v, want %v", nics[0].IPs, want)
	}
}

// agentCall is an Agent call with its cap, and an answer to it of n bytes
// (at least min) that it reads whole if it fits.
type agentCall struct {
	name, route string
	limit       int
	head, tail  string // the answer around its padding
	call        func(*Client) error
}

func (a agentCall) min() int            { return len(a.head) + len(a.tail) }
func (a agentCall) answer(n int) string { return a.head + strings.Repeat("x", n-a.min()) + a.tail }

// execCap is the cap the tests' callers give AgentExecStatus.
const execCap = 1 << 20

var agentCalls = []agentCall{
	{"AgentPing", "POST /nodes/cedar/qemu/2501/agent/ping", maxSmallAnswer, `{"data":{"result":{},"x":"`, `"}}`,
		func(c *Client) error { return c.AgentPing(context.Background(), "cedar", 2501) }},
	{"AgentRead", "POST /nodes/cedar/qemu/2501/agent/exec", maxSmallAnswer, `{"data":{"pid":7,"x":"`, `"}}`,
		func(c *Client) error {
			_, err := c.AgentRead(context.Background(), "cedar", 2501, []string{"true"})
			return err
		}},
	{"AgentExec", "POST /nodes/cedar/qemu/2501/agent/exec", maxSmallAnswer, `{"data":{"pid":7,"x":"`, `"}}`,
		func(c *Client) error {
			_, err := c.AgentExec(context.Background(), "cedar", 2501, []string{"true"}, "")
			return err
		}},
	{"AgentExecStatus", "GET /nodes/cedar/qemu/2501/agent/exec-status", execCap, `{"data":{"exited":1,"exitcode":0,"out-data":"`, `"}}`,
		func(c *Client) error {
			_, err := c.AgentExecStatus(context.Background(), "cedar", 2501, 7, execCap)
			return err
		}},
	{"AgentHostname", "GET /nodes/cedar/qemu/2501/agent/get-host-name", maxSmallAnswer, `{"data":{"result":{"host-name":"`, `"}}}`,
		func(c *Client) error { _, err := c.AgentHostname(context.Background(), "cedar", 2501); return err }},
	{"AgentInterfaces", "GET /nodes/cedar/qemu/2501/agent/network-get-interfaces", maxNICsAnswer, nicsHead, nicsTail,
		func(c *Client) error { _, err := c.AgentInterfaces(context.Background(), "cedar", 2501); return err }},
}

// Every Agent method is in agentCalls, so TestPropAgentAnswersCapped holds
// it to a cap.
func TestEveryAgentCallIsCapped(t *testing.T) {
	typ := reflect.TypeFor[*Client]()
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if strings.HasPrefix(name, "Agent") && !slices.ContainsFunc(agentCalls, func(a agentCall) bool { return a.name == name }) {
			t.Errorf("%s isn't in agentCalls", name)
		}
	}
}

// A caller that gives no cap gets an error, not a read of any size.
func TestAgentCallWithoutACapFails(t *testing.T) {
	f, c := newFake(t)
	f.routes["GET /nodes/cedar/qemu/2501/agent/exec-status"] = jsonData(`{"exited":1,"exitcode":0}`)
	for _, limit := range []int64{0, -1} {
		if _, err := c.AgentExecStatus(context.Background(), "cedar", 2501, 7, limit); err == nil || !strings.Contains(err.Error(), "no cap") {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("requests sent: %q", f.requests)
	}
}

// Every answer to an Agent call, which the guest writes, is read only up to
// that call's cap: one that fits is read, a longer one refused.
func TestPropAgentAnswersCapped(t *testing.T) {
	f, c := newFake(t)
	var mu sync.Mutex
	answer := ""
	serve := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(w, answer)
	}
	for _, a := range agentCalls {
		f.routes[a.route] = serve
	}
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.SampledFrom(agentCalls).Draw(t, "call")
		n := rapid.OneOf(rapid.IntRange(a.limit-64, a.limit+64), rapid.IntRange(a.min(), 4*a.limit)).Draw(t, "bytes")
		mu.Lock()
		answer = a.answer(n)
		mu.Unlock()
		err := a.call(c)
		if fits := n <= a.limit; fits != (err == nil) {
			t.Fatalf("%s, an answer of %d bytes: %v", a.name, n, err)
		}
		if err != nil && !strings.Contains(err.Error(), "answer over "+strconv.Itoa(a.limit)+" bytes") {
			t.Fatalf("%s, an answer of %d bytes: %v", a.name, n, err)
		}
	})
}

// What the agent writes for an address reads back as that address, with
// the prefix length it wrote if that is a whole number the address can
// have, else -1. An IPv4 address written as IPv6 reads back as the IPv4
// address, with that length less 96, or -1 if it is under 96. An address
// that doesn't parse, or has a zone, is left out.
func TestPropAgentInterfacesAddrs(t *testing.T) {
	f, c := newFake(t)
	var mu sync.Mutex
	answer := ""
	f.routes["GET /nodes/cedar/qemu/2501/agent/network-get-interfaces"] = func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(w, answer)
	}
	rapid.Check(t, func(t *rapid.T) {
		type wrote struct {
			addr, prefix string // as written; prefix "" for none
			want         netip.Prefix
			kept         bool
		}
		ips := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) wrote {
			var a, v4 netip.Addr
			switch rapid.IntRange(0, 2).Draw(t, "family") {
			case 0:
				a = netip.AddrFrom4([4]byte(rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, "v4")))
			case 1:
				a = netip.AddrFrom16([16]byte(rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "v6")))
			default:
				v4 = netip.AddrFrom4([4]byte(rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, "v4 in v6")))
				b := [16]byte{10: 0xff, 11: 0xff}
				copy(b[12:], v4.AsSlice())
				a = netip.AddrFrom16(b)
			}
			text := a.String()
			if rapid.Bool().Draw(t, "upper") {
				text = strings.ToUpper(text)
			}
			w := wrote{addr: text, kept: true}
			bits := -1
			switch rapid.IntRange(0, 3).Draw(t, "prefix") {
			case 0: // none
			case 1:
				p := rapid.IntRange(-200, 200).Draw(t, "bits")
				w.prefix = strconv.Itoa(p)
				if p >= 0 && p <= a.BitLen() {
					bits = p
				}
			case 2:
				w.prefix = rapid.SampledFrom([]string{"24.0", "2.4e1", "1e400", "99999999999999999999", `"24"`, "null"}).Draw(t, "odd")
				if w.prefix == `"24"` && a.BitLen() >= 24 {
					bits = 24
				}
			default:
				w.addr = rapid.SampledFrom([]string{"", "x", text + "%eth0", text + " ", "<b>" + text + "</b>", text + "\u200b", "010.050.012.133"}).Draw(t, "unread")
				w.kept = false
			}
			w.want = netip.PrefixFrom(a, bits)
			if v4.IsValid() {
				w.want = netip.PrefixFrom(v4, -1)
				if bits >= 96 {
					w.want = netip.PrefixFrom(v4, bits-96)
				}
			}
			return w
		}), 0, 8).Draw(t, "ips")
		var parts []string
		var want []netip.Prefix
		for _, ip := range ips {
			part := `{"ip-address":"` + ip.addr + `"`
			if ip.prefix != "" {
				part += `,"prefix":` + ip.prefix
			}
			parts = append(parts, part+"}")
			if ip.kept {
				want = append(want, ip.want)
			}
		}
		mu.Lock()
		answer = `{"data":{"result":[{"name":"eth0","hardware-address":"BC:24:11:00:00:01","ip-addresses":[` + strings.Join(parts, ",") + `]}]}}`
		mu.Unlock()
		nics, err := c.AgentInterfaces(context.Background(), "cedar", 2501)
		if err != nil || len(nics) != 1 {
			t.Fatalf("%d NICs, %v", len(nics), err)
		}
		if !slices.Equal(nics[0].IPs, want) {
			t.Fatalf("addresses %v, want %v", nics[0].IPs, want)
		}
	})
}

// nicsHead and nicsTail are an answer to AgentInterfaces around its NIC's
// name.
const (
	nicsHead = `{"data":{"result":[{"name":"`
	nicsTail = `","hardware-address":"BC:24:11:00:00:01","ip-addresses":[{"ip-address":"192.168.100.10","prefix":24}]}]}}`
)
