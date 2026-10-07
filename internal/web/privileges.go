package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/status"
)

// Proxmox decides what each user may do. Pages offer an action only if the
// user holds its privilege somewhere (pods.OfferPrivileges). This is for
// clarity, not security: previews check exactly (pods.BlockUnpermitted) and
// Proxmox checks every call.

// gridOps are the operations the grid's action bar offers on ticked VMs.
var gridOps = []pods.Kind{pods.KindPower, pods.KindReset, pods.KindSnapshot}

// needs is kind's offering privileges, space-separated, for the action bar's
// data-needs.
func needs(kind string) string { return strings.Join(pods.OfferPrivileges(pods.Kind(kind)), " ") }

// mayAnywhere reports whether the user holds any of privs on any path Proxmox
// lists for them. Without a privilege reader (test fakes) it says yes.
func (s *Server) mayAnywhere(ctx context.Context, privs ...string) bool {
	acc := s.accessFor(ctx)
	if acc == nil {
		return true
	}
	for _, p := range privs {
		if ok, err := acc.Anywhere(ctx, p); err == nil && ok {
			return true
		}
	}
	return false
}

// mayOn reports whether the user holds any of privs on VM vmid, as far as the
// listed paths tell (UserAccess.Approx).
func (s *Server) mayOn(ctx context.Context, vmid int, privs ...string) bool {
	acc := s.accessFor(ctx)
	if acc == nil {
		return true
	}
	held, err := acc.Approx(ctx, pods.VMPath(vmid))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(privs, func(p string) bool { _, ok := held[p]; return ok })
}

// lacks lists the grid actions' privileges the user lacks on VM vmid, for
// data-lacks.
func (s *Server) lacks(ctx context.Context, vmid int) []string {
	var out []string
	for _, k := range gridOps {
		for _, p := range pods.OfferPrivileges(k) {
			if !slices.Contains(out, p) && !s.mayOn(ctx, vmid, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// mayRun reports whether the user holds kind's privilege anywhere, i.e.
// whether to offer it.
func (s *Server) mayRun(ctx context.Context, kind pods.Kind) bool {
	return s.mayAnywhere(ctx, pods.OfferPrivileges(kind)...)
}

// require serves next only to a signed-in user with a Proxmox ticket
// (auth.RequireUser). The user's grid opens on first use (view) and is held
// until the request ends.
func (s *Server) require(next http.Handler) http.Handler {
	return s.auth.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rv := &requestView{}
		defer rv.release()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestViewKey{}, rv)))
	}))
}

// requestView is a request's hold on its user's grid.
type requestView struct {
	once sync.Once
	v    *status.View
	done func()
}

type requestViewKey struct{}

func (rv *requestView) release() {
	rv.once.Do(func() {}) // nobody opened it, and nobody can now
	if rv.done != nil {
		rv.done()
	}
}

// view is the signed-in user's grid (see status.Views), read with their
// ticket. It lingers after the request so the page's event stream can take it
// over. Only requests through require have one.
func (s *Server) view(ctx context.Context) *status.View {
	rv, ok := ctx.Value(requestViewKey{}).(*requestView)
	if !ok {
		panic("web: view outside require")
	}
	rv.once.Do(func() {
		cred, _ := auth.ProxmoxCredential(ctx)
		rv.v, rv.done = s.views.Open(cred.User, cred)
	})
	return rv.v
}
