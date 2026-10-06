package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// do calls the API at the active endpoint and, when that fails in a way
// that makes it safe to send the call again elsewhere, at the others in
// turn, starting after the active one and wrapping around, each at most
// once. The first endpoint that answers becomes the active one, and the
// switch is logged once.
//
// Safety: a write (POST, PUT, DELETE) is sent to another endpoint only if
// it provably never reached the first: the connection was never made
// (refused, no route, DNS) or the TLS handshake failed. A write that may
// have arrived (a timeout or reset after sending, or any HTTP answer) is
// never sent again here, or a clone or delete could run twice; the caller's
// Retrier decides about it. Reads are also sent again after any
// other connection error, a timeout, or a proxy-level 5xx (see proxyDown).
//
// With one endpoint, errors come back unchanged. When every endpoint fails,
// the error is an *EndpointsError naming them all; it wraps each failure,
// so errors.As and the Classifier see through it.
func (c *Client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	_, err := c.doServed(ctx, method, path, params, out)
	if c.refused != nil && IsLapsed(err) {
		c.refused()
	}
	return err
}

// doPinned calls the API at the endpoint whose host:port is host, and
// nowhere else: for calls that only the node that served an earlier one
// can answer.
func (c *Client) doPinned(ctx context.Context, host, method, path string, params url.Values, out any) error {
	for _, e := range c.endpoints {
		if e.host == host {
			_, err := c.doAt(ctx, Credential{}, e.base, method, path, params, out)
			return err
		}
	}
	return fmt.Errorf("%s %s: %s is not one of the configured Proxmox endpoints", method, path, host)
}

// doServed is do, and also says which endpoint (host:port) answered.
func (c *Client) doServed(ctx context.Context, method, path string, params url.Values, out any) (string, error) {
	var cred Credential
	if !c.anon {
		if c.cred != nil {
			cred = c.cred()
		}
		if !cred.Usable() {
			return "", fmt.Errorf("%s %s: %w", method, path, ErrNoCredential)
		}
	}
	n := len(c.endpoints)
	start := int(c.active.Load())
	var (
		failed []endpointFailure
		first  string // why start failed, for the log
	)
	for i := range n {
		idx := (start + i) % n
		transport, err := c.doAt(ctx, cred, c.endpoints[idx].base, method, path, params, out)
		answered := err == nil || !transport // the endpoint sent an HTTP answer
		reason, eligible := failoverReason(method, err, transport)
		if err == nil || !eligible || n == 1 || ctx.Err() != nil {
			if i > 0 && answered {
				c.switchTo(start, idx, first)
			}
			return c.endpoints[idx].host, err
		}
		if i == 0 {
			first = reason
		}
		failed = append(failed, endpointFailure{host: c.endpoints[idx].host, reason: reason, err: err})
	}
	return "", &EndpointsError{Method: method, Path: path, Failures: failed}
}

// switchTo makes to the active endpoint if from still is, and logs it.
// Calls failing at the same time all try; one wins and logs.
func (c *Client) switchTo(from, to int, reason string) {
	if !c.active.CompareAndSwap(int64(from), int64(to)) {
		return
	}
	if c.logf != nil {
		c.logf("proxmox: failing over from %s to %s: %s", c.endpoints[from].host, c.endpoints[to].host, reason)
	}
}

type endpointFailure struct {
	host   string
	reason string
	err    error
}

// EndpointsError is a call that failed at every endpoint.
type EndpointsError struct {
	Method, Path string
	Failures     []endpointFailure
}

func (e *EndpointsError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = f.host + ": " + f.reason
	}
	return fmt.Sprintf("%s %s: every Proxmox endpoint failed: %s", e.Method, e.Path, strings.Join(parts, "; "))
}

func (e *EndpointsError) Unwrap() []error {
	out := make([]error, len(e.Failures))
	for i, f := range e.Failures {
		out[i] = f.err
	}
	return out
}

// proxyDown reports whether a status is pveproxy (or a proxy in front of
// it) saying it couldn't reach the API, rather than the API answering:
// 502/503/504, and Proxmox's 595 (connection failed), 596 (node
// unreachable or timed out) and 599. Plain 500 is not: Proxmox uses it for
// ordinary errors such as a missing VM config, which another node would
// answer the same way.
func proxyDown(status int) bool {
	switch status {
	case 502, 503, 504, 595, 596, 599:
		return true
	}
	return false
}

// failoverReason reports whether err from method makes it safe to try the
// call at another endpoint, and a short reason for the log. transport says
// err came from the connection, not from an HTTP answer.
func failoverReason(method string, err error, transport bool) (string, bool) {
	if err == nil {
		return "", false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if method == http.MethodGet && proxyDown(apiErr.Status) {
			return fmt.Sprintf("HTTP %d %s", apiErr.Status, apiErr.Message), true
		}
		return "", false
	}
	if !transport || errors.Is(err, context.Canceled) {
		return "", false
	}
	if reason, ok := neverSent(err); ok {
		return reason, true
	}
	if method != http.MethodGet {
		return "", false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out", true
	}
	return transportText(err), true
}

// neverSent recognizes the errors that prove a request never left: the
// connection was never made, or its TLS handshake failed (Go sends the
// request only after the handshake).
func neverSent(err error) (string, bool) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS: " + dnsErr.Error(), true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "can't connect: " + innerText(opErr), true
	}
	var (
		verifyErr  *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
		recordErr  tls.RecordHeaderError
		alertErr   tls.AlertError
	)
	if errors.As(err, &verifyErr) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) ||
		errors.As(err, &invalidErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) {
		return "TLS handshake failed: " + transportText(err), true
	}
	msg := err.Error()
	for _, s := range []string{"TLS handshake timeout", "server gave HTTP response to HTTPS client"} {
		if strings.Contains(msg, s) {
			return "TLS handshake failed: " + s, true
		}
	}
	return "", false
}

// innerText is an OpError's cause, e.g. "connect: connection refused".
func innerText(e *net.OpError) string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Error()
}

// transportText is a connection error without the request URL that
// url.Error puts in front.
func transportText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}
