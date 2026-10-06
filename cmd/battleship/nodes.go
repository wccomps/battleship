package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"strings"
	"text/tabwriter"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// nodeLister is the part of the Proxmox client "battleship nodes" uses.
type nodeLister interface {
	ClusterNodes(ctx context.Context) ([]proxmox.ClusterNode, error)
}

// runNodes is "battleship nodes": it lists the cluster's nodes from
// /cluster/status and prints a proxmox.urls line to paste into the config.
// It only reads.
func runNodes(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet("battleship nodes", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	cfgPath := fs.String("config", "battleship.toml", "config file")
	if code, ok := parseFlags(fs, args, 0, d); !ok {
		return code
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	token, err := userToken()
	if err != nil {
		fmt.Fprintln(d.stderr, err)
		return 1
	}
	client := d.newAPI(cfg.Proxmox)
	setProxmoxLogf(client, func(format string, a ...any) { fmt.Fprintf(d.stderr, format+"\n", a...) })
	lister, ok := asToken(client, token).(nodeLister)
	if !ok {
		fmt.Fprintln(d.stderr, "this Proxmox client can't list nodes")
		return 1
	}
	nodes, err := lister.ClusterNodes(ctx)
	if err != nil {
		fmt.Fprintln(d.stderr, "listing nodes:", proxmox.Describe(err))
		return 1
	}
	tw := tabwriter.NewWriter(d.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tSTATUS\tIP")
	var urls []string
	for _, n := range nodes {
		status := "offline"
		if n.Online {
			status = "online"
		}
		ip := n.IP
		if ip == "" {
			ip = "-"
		} else {
			urls = append(urls, fmt.Sprintf("%q", "https://"+net.JoinHostPort(n.IP, "8006")))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", n.Name, status, ip)
	}
	tw.Flush()
	fmt.Fprintf(d.stdout, "\nPaste under [proxmox] in place of url (battleship uses one at a time and fails over in this order):\nurls = [%s]\n", strings.Join(urls, ", "))
	return 0
}

// setProxmoxLogf sends the client's failover lines to logf, if api is a
// client that logs them.
func setProxmoxLogf(api any, logf func(format string, args ...any)) {
	if l, ok := api.(interface {
		SetLogf(func(format string, args ...any))
	}); ok {
		l.SetLogf(logf)
	}
}
