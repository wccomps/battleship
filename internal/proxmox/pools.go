package proxmox

import (
	"context"
	"net/http"
	"net/url"
)

// Pool is a resource pool as GET /pools lists it.
type Pool struct {
	ID      string `json:"poolid"`
	Comment string `json:"comment"`
}

// Pools lists the pools the caller holds Pool.Audit on, with their comments;
// Proxmox leaves the others out.
func (c *Client) Pools(ctx context.Context) ([]Pool, error) {
	var out []Pool
	if err := c.do(ctx, http.MethodGet, "/pools", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetPoolComment replaces pool's comment; it needs Pool.Allocate on
// /pool/<pool>.
func (c *Client) SetPoolComment(ctx context.Context, pool, comment string) error {
	return c.do(ctx, http.MethodPut, "/pools", url.Values{"poolid": {pool}, "comment": {comment}}, nil)
}
