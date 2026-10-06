// Package proxmox is a small typed client for the Proxmox VE API.
package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Client talks to one Proxmox cluster through one or more of its nodes'
// API endpoints. It uses one active endpoint, sticky across calls, and
// fails over to the next when a call can safely be sent again elsewhere
// (see failover.go).
//
// A Client holds no credential of its own: New's client can only start
// and finish logins (OpenIDAuthURL, OpenIDLogin, RenewTicket), and every
// other call is made through a view, As(cred) or AsSource(src), that sends
// a person's credential. Views share the endpoints, the active endpoint and
// the connections.
type Client struct {
	*conn
	// cred is the view's credential, read for each request; nil for the
	// client New returns, whose calls fail with ErrNoCredential.
	cred func() Credential
	anon bool // the call needs no credential (logins and renewals)
	// refused, if set, is called each time Proxmox refuses the view's
	// credential (401); see WhenRefused.
	refused func()
}

// conn is what every view of a client shares.
type conn struct {
	endpoints []endpoint
	active    atomic.Int64 // index into endpoints
	http      *http.Client
	logf      func(format string, args ...any)
}

// As is a view of c that makes every call as cred.
func (c *Client) As(cred Credential) *Client {
	return &Client{conn: c.conn, cred: func() Credential { return cred }}
}

// AsSource is a view of c that makes each call with the credential src
// returns at the time, so a renewed ticket is used as soon as src has it.
func (c *Client) AsSource(src func() Credential) *Client {
	return &Client{conn: c.conn, cred: src}
}

// WhenRefused is a view of c that also calls refused each time Proxmox
// refuses its credential (401: the ticket expired or the user's access was
// revoked), so its owner can stop at once.
func (c *Client) WhenRefused(refused func()) *Client {
	v := *c
	v.refused = refused
	return &v
}

// anonymous is a view that sends no credential, for logins.
func (c *Client) anonymous() *Client { return &Client{conn: c.conn, anon: true} }

// endpoint is one node's API: base is https://host:8006/api2/json and host
// is host:8006, which is what logs and errors show.
type endpoint struct {
	base string
	host string
}

type Options struct {
	// URLs are the cluster's nodes, e.g. https://192.0.2.123:8006, tried in
	// order (see failover.go).
	URLs               []string
	InsecureSkipVerify bool
	// RootCAs, if set, are the CAs Proxmox's certificate is checked
	// against (e.g. the cluster's CA) instead of the system's. Each node's
	// certificate names that node, so one pool verifies them all.
	RootCAs *x509.CertPool
}

// requestTimeout bounds each request.
const requestTimeout = 60 * time.Second

func New(o Options) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Proxmox's certificate is self-signed unless the cluster CA is given
	// (proxmox.ca_file); config refuses ca_file with insecure_skip_verify.
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: o.InsecureSkipVerify, RootCAs: o.RootCAs} //nolint:gosec // see above
	tr.MaxIdleConnsPerHost = 32                                                                    // the executor runs many calls in parallel
	c := &Client{conn: &conn{http: &http.Client{Transport: tr, Timeout: requestTimeout}}}
	for _, raw := range o.URLs {
		raw = strings.TrimRight(raw, "/")
		host := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Host
		}
		c.endpoints = append(c.endpoints, endpoint{base: raw + "/api2/json", host: host})
	}
	return c
}

// SetLogf sets where failovers are logged, one line each. Call it before
// the client's first request.
func (c *Client) SetLogf(logf func(format string, args ...any)) { c.logf = logf }

// Endpoints lists every endpoint's host:port, in the configured order.
func (c *Client) Endpoints() []string {
	out := make([]string, len(c.endpoints))
	for i, e := range c.endpoints {
		out[i] = e.host
	}
	return out
}

// doAt calls the API at one endpoint's base. GET and DELETE send params as
// a query string; POST and PUT send them form-encoded. The response's
// "data" field is decoded into out when out is non-nil. transport reports
// that err came from the connection rather than from an answer.
func (c *Client) doAt(ctx context.Context, cred Credential, base, method, path string, params url.Values, out any) (transport bool, err error) {
	u := base + path
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	} else {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return false, err
	}
	if !c.anon {
		cred.authorize(req)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return true, err
	}
	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors map[string]string `json:"errors"`
	}
	unmarshalErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Proxmox puts the error text in the status line.
		msg := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
		// Go's net/http server (httptest, or a proxy written in Go) writes
		// "status code NNN" for codes it has no text for; treat as empty.
		if msg == "" || msg == "status code "+strconv.Itoa(resp.StatusCode) {
			// A body that isn't Proxmox's JSON (a proxy's error page) is
			// shown, on one line and cut to 200 bytes of valid UTF-8.
			if unmarshalErr != nil {
				msg = strings.Join(strings.Fields(string(raw)), " ")
				if len(msg) > 200 {
					msg = strings.ToValidUTF8(msg[:200], "")
				}
			}
			if msg == "" {
				msg = http.StatusText(resp.StatusCode)
			}
		}
		if len(envelope.Errors) > 0 {
			for _, k := range slices.Sorted(maps.Keys(envelope.Errors)) {
				msg += fmt.Sprintf("; %s: %s", k, strings.TrimSpace(envelope.Errors[k]))
			}
		}
		return false, &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: msg}
	}
	if unmarshalErr != nil && out != nil {
		return false, fmt.Errorf("%s %s: invalid JSON response: %w", method, path, unmarshalErr)
	}
	if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(envelope.Data))
	dec.UseNumber()
	return false, dec.Decode(out)
}

func vmPath(node string, vmid int) string {
	return fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(node), vmid)
}
