package core

import (
	"context"
	"time"

	"github.com/oboo/moho/internal/i18n"
	"github.com/oboo/moho/internal/mihomo"
)

// NewExternal prepares a manager of a core that someone else runs, such as
// a sing-box with clash_api on addr. It starts nothing and never writes
// the core's config; Connect attaches to it.
func NewExternal(addr, secret string) *Manager {
	return &Manager{
		status: Status{State: Stopped}, external: true,
		tcpAddr: addr, secret: secret, PollEvery: 2 * time.Second,
	}
}

// External reports whether the core is someone else's, so that bowel
// must neither write its config nor change what it takes over.
func (m *Manager) External() bool { return m.external }

// Connect attaches to the external controller and keeps following it: it
// is reported Failed when the controller goes away (the core restarts on
// every config change) and Running again when it is back. The returned
// error is the first attempt's; the manager retries either way.
func (m *Manager) Connect() error {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	m.setStatus(Status{State: Starting})
	c := m.newClient()
	if m.OnTraffic != nil {
		go m.traffic(ctx, c)
	}
	err := m.probe(ctx, c)
	go m.watch(ctx, c)
	return err
}

func (m *Manager) watch(ctx context.Context, c *mihomo.Client) {
	t := time.NewTicker(m.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.probe(ctx, c)
		}
	}
}

func (m *Manager) probe(ctx context.Context, c *mihomo.Client) error {
	cx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, err := c.Version(cx)
	if err == nil {
		err = proxiesLoaded(cx, c)
	}
	if err != nil {
		m.report(ctx, Status{State: Failed, Error: i18n.T("无法连接外部内核: ", "Cannot reach the external core: ") + err.Error()}, nil)
		return err
	}
	m.report(ctx, Status{State: Running, Version: v.Version, StartedAt: time.Now()}, c)
	return nil
}

// report records a probe's outcome unless the manager was stopped meanwhile
// or the state did not change. The check and the write share one lock so
// that a probe cannot overwrite the Stopped that Stop just set.
func (m *Manager) report(ctx context.Context, s Status, c *mihomo.Client) {
	m.mu.Lock()
	if ctx.Err() != nil || m.status.State == s.State {
		m.mu.Unlock()
		return
	}
	m.client, m.status = c, s
	fn := m.OnStatus
	m.mu.Unlock()
	if fn != nil {
		fn(s)
	}
}

// stopExternal detaches from the controller; the core itself keeps running.
func (m *Manager) stopExternal() {
	m.mu.Lock()
	if m.cancel == nil {
		m.mu.Unlock()
		return
	}
	m.cancel()
	m.cancel, m.client = nil, nil
	m.mu.Unlock()
	m.setStatus(Status{State: Stopped})
}
