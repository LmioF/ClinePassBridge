package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type egressGroup struct {
	rpm, maxInflight int
	inflight         int
	sent             []time.Time
	cooldown         time.Time
}

type egressContextBinding struct{ ctx context.Context }

type egressOperation struct {
	callback string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	deadline context.CancelFunc
}

type egressManager struct {
	saveMu     sync.Mutex
	mu         sync.Mutex
	cfg        EgressConfig
	groups     map[string]*egressGroup
	clients    map[string]*http.Client
	streams    map[string]*egressLocalStream
	operations map[*egressOperation]struct{}
	bindings   map[string]*egressContextBinding
	changed    chan struct{}
	closed     bool
	cursor     int
	clock      func() time.Time
}

func newEgressManager() *egressManager {
	return &egressManager{cfg: defaultEgressConfig(), groups: map[string]*egressGroup{}, clients: map[string]*http.Client{}, streams: map[string]*egressLocalStream{}, operations: map[*egressOperation]struct{}{}, bindings: map[string]*egressContextBinding{}, changed: make(chan struct{})}
}

func (p *egressManager) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

func (p *egressManager) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *egressManager) apply(cfg EgressConfig) {
	p.mu.Lock()
	clients := p.clients
	p.clients = map[string]*http.Client{}
	p.cfg = cfg
	// Preserve rolling usage/cooldowns across edits, including active leases.
	for _, e := range cfg.Endpoints {
		if g := p.groups[e.Group]; g != nil {
			g.rpm, g.maxInflight = e.RPM, e.MaxInflight
		}
	}
	p.notifyLocked()
	p.mu.Unlock()
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}

func pruneEgressUsage(g *egressGroup, now time.Time) {
	cutoff := now.Add(-time.Minute)
	n := 0
	for n < len(g.sent) && !g.sent[n].After(cutoff) {
		n++
	}
	if n > 0 {
		copy(g.sent, g.sent[n:])
		g.sent = g.sent[:len(g.sent)-n]
	}
}

// Explicit credential proxies take precedence. Unlisted proxies are grouped by
// their gateway address, conservatively sharing capacity across auth variants.
// Operators should configure explicit physical-IP groups for rotating gateways.
func (p *egressManager) candidatesLocked(pin string) ([]EgressEndpoint, error) {
	if pin == "" {
		if !p.cfg.Enabled {
			return nil, nil
		}
		var enabled []EgressEndpoint
		for _, endpoint := range p.cfg.Endpoints {
			if endpoint.Enabled {
				enabled = append(enabled, endpoint)
			}
		}
		return enabled, nil
	}
	proxy, err := normalizeEgressProxy(pin)
	if err != nil {
		return nil, err
	}
	var matching []EgressEndpoint
	known := false
	for _, endpoint := range p.cfg.Endpoints {
		if endpoint.ProxyURL == proxy {
			known = true
			if endpoint.Enabled {
				matching = append(matching, endpoint)
			}
		}
	}
	if known {
		return matching, nil // A disabled configured proxy is never resurrected.
	}
	if proxy == "direct" {
		return nil, fail(400, "direct credential routing requires an enabled direct pool endpoint")
	}
	u, _ := url.Parse(proxy)
	u.User = nil
	hash := sha256.Sum256([]byte(u.String()))
	group := "credential-proxy-" + hex.EncodeToString(hash[:8])
	return []EgressEndpoint{{ID: group, Group: group, ProxyURL: proxy, Enabled: true, RPM: 60, MaxInflight: 4}}, nil
}

type egressLease struct {
	pool     *egressManager
	group    *egressGroup
	endpoint EgressEndpoint
	reserved time.Time
	sent     bool
	once     sync.Once
}

func (lease *egressLease) release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		p := lease.pool
		p.mu.Lock()
		lease.group.inflight--
		// Cancellation before Client.Do refunds the unused admission. An actual
		// network attempt, even a failed dial, continues to consume RPM.
		if !lease.sent {
			for i, at := range lease.group.sent {
				if at == lease.reserved {
					lease.group.sent = append(lease.group.sent[:i], lease.group.sent[i+1:]...)
					break
				}
			}
		}
		p.notifyLocked()
		p.mu.Unlock()
	})
}

func (p *egressManager) acquire(ctx context.Context, pin string) (*egressLease, error) {
	p.mu.Lock()
	wait := time.Duration(p.cfg.WaitTimeoutSeconds) * time.Second
	p.mu.Unlock()
	limit := time.NewTimer(wait)
	defer limit.Stop()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, fail(503, "egress transport is closing")
		}
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, egressContextError(ctx)
		}
		endpoints, err := p.candidatesLocked(pin)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		now := p.now()
		var next time.Time
		for n := 0; n < len(endpoints); n++ {
			i := (p.cursor + n) % len(endpoints)
			e := endpoints[i]
			g := p.groups[e.Group]
			if g == nil {
				g = &egressGroup{rpm: e.RPM, maxInflight: e.MaxInflight}
				p.groups[e.Group] = g
			}
			pruneEgressUsage(g, now)
			available := g.cooldown
			if len(g.sent) >= g.rpm {
				// A lowered RPM may require more than the oldest request to expire.
				available = laterTime(available, g.sent[len(g.sent)-g.rpm].Add(time.Minute))
			}
			if !available.After(now) && g.inflight < g.maxInflight {
				g.inflight++
				g.sent = append(g.sent, now)
				p.cursor = (i + 1) % len(endpoints)
				p.mu.Unlock()
				return &egressLease{pool: p, group: g, endpoint: e, reserved: now}, nil
			}
			if !available.After(now) {
				available = now.Add(time.Second) // Active bodies wake waiters on close.
			}
			if next.IsZero() || available.Before(next) {
				next = available
			}
		}
		if next.IsZero() {
			next = now.Add(time.Second)
		}
		changed := p.changed
		p.mu.Unlock()
		local := &upstreamError{APIError: &APIError{429, "local_egress_unavailable", "[clinepassbridge:egress_unavailable] No egress capacity available; no upstream request sent"}, RetryAt: next, Scope: "egress_pool", Local: true}
		if wait == 0 {
			return nil, local
		}
		timer := time.NewTimer(max(time.Millisecond, next.Sub(now)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, egressContextError(ctx)
		case <-limit.C:
			timer.Stop()
			return nil, local
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func egressContextError(ctx context.Context) error {
	if context.Cause(ctx) == context.DeadlineExceeded || ctx.Err() == context.DeadlineExceeded {
		return requestTimedOut("egress request deadline exceeded")
	}
	return requestCanceled("egress request canceled")
}

func (p *egressManager) bindContext(callback string, ctx context.Context) func() {
	if callback == "" || ctx == nil {
		return func() {}
	}
	b := &egressContextBinding{ctx}
	p.mu.Lock()
	p.bindings[callback] = b
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.bindings[callback] == b {
			delete(p.bindings, callback)
		}
		p.mu.Unlock()
	}
}

func (p *egressManager) cancelCallback(callback string) {
	if callback == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for op := range p.operations {
		if op.callback == callback {
			op.cancel(context.Canceled)
		}
	}
}

func (p *egressManager) operation(callback string, deadline time.Time) (*egressOperation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fail(503, "egress transport is closing")
	}
	parent := context.Background()
	if binding := p.bindings[callback]; binding != nil {
		parent = binding.ctx
	}
	ctx, deadlineCancel := context.WithDeadline(parent, deadline)
	ctx, cancel := context.WithCancelCause(ctx)
	op := &egressOperation{callback, ctx, cancel, deadlineCancel}
	p.operations[op] = struct{}{}
	return op, nil
}

func (p *egressManager) finishOperation(op *egressOperation) {
	op.cancel(context.Canceled)
	op.deadline()
	p.mu.Lock()
	delete(p.operations, op)
	p.mu.Unlock()
}

func (p *egressManager) close() {
	p.mu.Lock()
	p.closed = true
	for op := range p.operations {
		op.cancel(context.Canceled)
	}
	streams := make([]*egressLocalStream, 0, len(p.streams))
	for _, stream := range p.streams {
		streams = append(streams, stream)
	}
	clients := p.clients
	p.clients = map[string]*http.Client{}
	p.bindings = map[string]*egressContextBinding{}
	p.notifyLocked()
	p.mu.Unlock()
	for _, stream := range streams {
		stream.close()
	}
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}

func (s *Service) egressStatus() map[string]any {
	p := s.getEgressManager()
	p.mu.Lock()
	defer p.mu.Unlock()
	groups := []map[string]any{}
	seen := map[string]bool{}
	for _, e := range p.cfg.Endpoints {
		if seen[e.Group] {
			continue
		}
		seen[e.Group] = true
		g := p.groups[e.Group]
		row := map[string]any{"group": e.Group, "rpm": e.RPM, "max_inflight": e.MaxInflight, "used_last_60s": 0, "inflight": 0}
		if g != nil {
			pruneEgressUsage(g, p.now())
			row["used_last_60s"], row["inflight"] = len(g.sent), g.inflight
			if g.cooldown.After(p.now()) {
				row["cooldown_until"] = g.cooldown.UTC()
			}
		}
		groups = append(groups, row)
	}
	return map[string]any{"enabled": p.cfg.Enabled, "groups": groups}
}
