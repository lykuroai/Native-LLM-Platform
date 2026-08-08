// Package pool manages deployment nodes as one logical resource (§9)。
//
// Phase 1 の分散 Mode は Request Sharding(request 単位分散)。node は
// deployment(connector endpoint または engine instance)を指す。Sticky
// Routing は最適化であり正しい会話記憶の条件にしない(§9.5)— node 障害時は
// Conversation Memory から context を再構築する。
package pool

import (
	"sort"
	"sync"
	"time"
)

// Node is the runtime view of one deployment (§9.3 の報告項目の in-process 版)。
type Node struct {
	ID        string
	RouteType string // native | connector
	Healthy   bool
	Draining  bool
	Inflight  int
	// EMALatencyMS is an exponential moving average of request latency。
	EMALatencyMS float64
	LastSeen     time.Time
}

type stickyEntry struct {
	nodeID  string
	expires time.Time
}

// Pool tracks node state and orders candidates for the scheduler (§9.4)。
type Pool struct {
	mu        sync.Mutex
	nodes     map[string]*Node
	sticky    map[string]stickyEntry // conversation key -> node
	stickyTTL time.Duration
}

// New builds an empty pool。stickyTTL <= 0 は sticky 無効。
func New(stickyTTL time.Duration) *Pool {
	return &Pool{
		nodes:     map[string]*Node{},
		sticky:    map[string]stickyEntry{},
		stickyTTL: stickyTTL,
	}
}

// Register adds or refreshes a node。健全性は明示報告(SetHealth)まで true。
func (p *Pool) Register(id, routeType string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.nodes[id]; ok {
		n.RouteType = routeType
		return
	}
	p.nodes[id] = &Node{ID: id, RouteType: routeType, Healthy: true, LastSeen: time.Now()}
}

// SetHealth records a probe result。
func (p *Pool) SetHealth(id string, healthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.nodes[id]; ok {
		n.Healthy = healthy
		n.LastSeen = time.Now()
	}
}

// Drain stops new assignments to the node (§13.3 drain)。
func (p *Pool) Drain(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.nodes[id]; ok {
		n.Draining = true
	}
}

// Resume re-enables assignments。
func (p *Pool) Resume(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.nodes[id]; ok {
		n.Draining = false
	}
}

// Snapshot returns a copy of node states (管理表示用、本文なし §9.3)。
func (p *Pool) Snapshot() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Assignable reports whether the node accepts new requests。
func (p *Pool) Assignable(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	return ok && n.Healthy && !n.Draining
}

// Acquire marks a request in flight and returns a release function that
// records the outcome(latency EMA と health を更新)。
func (p *Pool) Acquire(id string) func(latency time.Duration, ok bool) {
	p.mu.Lock()
	if n, exists := p.nodes[id]; exists {
		n.Inflight++
	}
	p.mu.Unlock()
	return func(latency time.Duration, ok bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		n, exists := p.nodes[id]
		if !exists {
			return
		}
		if n.Inflight > 0 {
			n.Inflight--
		}
		ms := float64(latency.Milliseconds())
		if n.EMALatencyMS == 0 {
			n.EMALatencyMS = ms
		} else {
			n.EMALatencyMS = 0.8*n.EMALatencyMS + 0.2*ms
		}
		if !ok {
			n.Healthy = false // 失敗は即時降格。health probe が回復させる
		}
		n.LastSeen = time.Now()
	}
}

// Order sorts candidate node IDs for one request (§9.4 選択条件のうち
// in-process で観測可能な 5(load状態は候補生成側)・8(queue)・9(latency)・
// 10(health)・11(sticky) を実装。Policy 条件は Model Manager 側で先に
// 適用済みであること)。
//
// convKey が非空で sticky が有効なら、同一会話は前回 node を先頭にする。
// 非 assignable node は末尾へ回す(完全除外はしない — 全滅時の最終候補として
// 透過エラーを得るため)。
func (p *Pool) Order(candidates []string, convKey string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stickyNode string
	if convKey != "" && p.stickyTTL > 0 {
		if e, ok := p.sticky[convKey]; ok {
			if time.Now().Before(e.expires) {
				stickyNode = e.nodeID
			} else {
				delete(p.sticky, convKey)
			}
		}
	}

	type scored struct {
		id    string
		order float64
		dead  bool
	}
	items := make([]scored, 0, len(candidates))
	for _, id := range candidates {
		n, ok := p.nodes[id]
		if !ok {
			items = append(items, scored{id: id})
			continue
		}
		s := scored{id: id, order: float64(n.Inflight)*1000 + n.EMALatencyMS}
		if !n.Healthy || n.Draining {
			s.dead = true
		}
		if id == stickyNode && !s.dead {
			s.order = -1 // sticky 優先
		}
		items = append(items, s)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].dead != items[j].dead {
			return !items[i].dead
		}
		return items[i].order < items[j].order
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out
}

// RecordSticky remembers the node that served a conversation (§9.5)。
func (p *Pool) RecordSticky(convKey, nodeID string) {
	if convKey == "" || p.stickyTTL <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sticky[convKey] = stickyEntry{nodeID: nodeID, expires: time.Now().Add(p.stickyTTL)}
	// 期限切れ entry の日和見掃除(unbounded 化防止)
	if len(p.sticky) > 4096 {
		now := time.Now()
		for k, e := range p.sticky {
			if now.After(e.expires) {
				delete(p.sticky, k)
			}
		}
	}
}
