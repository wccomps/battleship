package status

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// Views keeps one grid per watching Proxmox user, polled with that user's
// ticket so it shows exactly what Proxmox lets them see. A view lives while
// streams hold it plus a linger, so a page's stream takes over the view the
// page was rendered from. Nobody watching means no polling.
type Views struct {
	bind   func(cred func() proxmox.Credential) pods.API
	hist   History
	lim    *apply.Limits
	cfg    config.Config
	opts   Options
	linger time.Duration
	clock  Clock

	life context.Context
	end  context.CancelFunc

	mu    sync.Mutex
	views map[string]*View
}

// View is one user's grid.
type View struct {
	*Poller
	User string

	mu    sync.Mutex
	cred  proxmox.Credential // the newest ticket a request of the user had
	refs  int
	gen   int // bumped by each Open, so a linger that started earlier knows
	stop  context.CancelFunc
	done  chan struct{}
	views *Views
}

// NewViews makes views whose pollers read the cluster through bind. linger
// is how long a view outlives its last stream.
func NewViews(bind func(cred func() proxmox.Credential) pods.API, hist History, lim *apply.Limits, cfg config.Config, opts Options, linger time.Duration) (*Views, error) {
	if bind == nil {
		return nil, errors.New("status: views need a way to read the cluster as someone")
	}
	// Check the config and the rest once, as a view would.
	if _, err := NewPoller(bind(func() proxmox.Credential { return proxmox.Credential{} }), hist, lim, cfg, opts); err != nil {
		return nil, err
	}
	vs := &Views{bind: bind, hist: hist, lim: lim, cfg: cfg, opts: opts, linger: linger, clock: opts.Clock, views: map[string]*View{}}
	if vs.clock == nil {
		vs.clock = SystemClock
	}
	vs.life, vs.end = context.WithCancel(context.Background())
	return vs, nil
}

// Close stops every view.
func (vs *Views) Close() { vs.end() }

// Open returns user's view (starting it if needed) and holds it until
// release. cred replaces the view's ticket if newer.
func (vs *Views) Open(user string, cred proxmox.Credential) (v *View, release func()) {
	vs.mu.Lock()
	v = vs.views[user]
	if v == nil {
		v = &View{User: user, cred: cred, done: make(chan struct{}), views: vs}
		p, err := NewPoller(vs.bind(v.credential), vs.hist, vs.lim, vs.cfg, vs.opts)
		if err != nil { // NewViews checked the same arguments
			panic(err)
		}
		v.Poller = p
		ctx, stop := context.WithCancel(vs.life)
		v.stop = stop
		go func() {
			defer close(v.done)
			if vs.opts.Manual {
				<-ctx.Done()
				return
			}
			p.Run(ctx)
		}()
		vs.views[user] = v
	}
	// Taken under vs.mu, which the linger holds while deciding to stop, so
	// it either sees this hold or already removed the view.
	v.mu.Lock()
	v.refs++
	v.gen++
	if cred.Usable() && cred.Issued.After(v.cred.Issued) {
		v.cred = cred
	}
	v.mu.Unlock()
	vs.mu.Unlock()
	var once sync.Once
	return v, func() { once.Do(v.release) }
}

func (v *View) credential() proxmox.Credential {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cred
}

// release drops one hold; the last starts the linger.
func (v *View) release() {
	v.mu.Lock()
	v.refs--
	if v.refs > 0 {
		v.mu.Unlock()
		return
	}
	gen := v.gen
	v.mu.Unlock()
	vs := v.views
	if vs.opts.Manual {
		return
	}
	after := vs.clock.After(vs.linger)
	go func() {
		select {
		case <-after:
		case <-v.done:
			return
		}
		vs.mu.Lock()
		defer vs.mu.Unlock()
		v.mu.Lock()
		idle := v.refs == 0 && v.gen == gen
		v.mu.Unlock()
		if idle && vs.views[v.User] == v {
			delete(vs.views, v.User)
			v.stop()
		}
	}()
}

// Done is closed once the view has stopped.
func (v *View) Done() <-chan struct{} { return v.done }

// WaitFirstPoll waits until the view's first good poll, or ctx ends.
func (v *View) WaitFirstPoll(ctx context.Context) error {
	select {
	case <-v.firstOK:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Each calls fn for every running view, in no particular order.
func (vs *Views) Each(fn func(*View)) {
	vs.mu.Lock()
	views := make([]*View, 0, len(vs.views))
	for _, v := range vs.views {
		views = append(views, v)
	}
	vs.mu.Unlock()
	for _, v := range views {
		fn(v)
	}
}
