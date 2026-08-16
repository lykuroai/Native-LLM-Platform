// Package platform assembles the Lykuro Native LLM Platform components
// (LYK-NLP-SD-001 v2.0)into the gateway-platform-v1 Backend。
//
// 構成(§1.1 のサービス境界を package 境界で維持):
//
//	contract        gateway-platform-v1(Gateway との唯一の境界)
//	enginecontract  platform-engine-v1(Engine との唯一の境界)
//	platformcfg     署名済み config の platform: 節
//	modelmanager    model catalog・deployment・Engine Control API 専有
//	connector       外部 Runtime への protocol 変換
//	orchestrator    route・admission・failover・conversation 統合
//	memory          Conversation Memory(暗号化・retention)
//	pool            node 状態・scheduler・sticky
//
// Gateway は本 package の New で得た Backend のみを参照し、内部 package へ
// 直接依存しない。
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/enginecontract"
	"github.com/lykuroai/Native-LLM-Platform/platform/memory"
	"github.com/lykuroai/Native-LLM-Platform/platform/modelmanager"
	"github.com/lykuroai/Native-LLM-Platform/platform/orchestrator"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
	"github.com/lykuroai/Native-LLM-Platform/platform/pool"
	"github.com/lykuroai/Native-LLM-Platform/platform/workflow"
)

// Platform is the assembled backend。
type Platform struct {
	cfg       *platformcfg.Config
	manager   *modelmanager.Manager
	orch      *orchestrator.Orchestrator
	pool      *pool.Pool
	mem       *memory.Store
	workflows *workflow.Service // nil = workflows 無効
	cancel    context.CancelFunc
}

var _ contract.Backend = (*Platform)(nil)

// Options allows test/future wiring of engine clients。
type Options struct {
	// Engines overrides engine bindings(test 用)。nil なら config の
	// engines[] を解決する("mock:" のみ対応 — 実 Engine は実装中)。
	Engines map[string]modelmanager.EnginePair
}

// New assembles and starts the platform for a validated config。
// cfg.Enabled=false では呼ばない(Gateway 側 flag が経路を守る)。
func New(cfg *platformcfg.Config, opts Options) (*Platform, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("platform: config is not enabled")
	}
	engines := opts.Engines
	if engines == nil {
		engines = map[string]modelmanager.EnginePair{}
		for _, e := range cfg.Engines {
			switch e.Endpoint {
			case "mock:":
				m := enginecontract.NewMockEngine()
				engines[e.ID] = modelmanager.EnginePair{Data: m, Control: m}
			default:
				// 実 Engine transport binding は Engine release 後に確定
				// (Phase 0 報告 §2)。未接続 engine の deployment は
				// unavailable として扱う — 未実装能力を公開しない(§8.3)。
				slog.Warn("engine endpoint not supported yet; deployments will be unavailable",
					"engine_id", e.ID)
			}
		}
	}

	p := pool.New(time.Duration(cfg.Pool.StickyTTLSeconds) * time.Second)
	mgr := modelmanager.New(cfg, p, engines)

	var mem *memory.Store
	if cfg.Memory.Mode == "managed" {
		var err error
		mem, err = memory.Open(cfg.Memory)
		if err != nil {
			return nil, err
		}
	}

	orch := orchestrator.New(cfg, mgr, p, mem)

	ctx, cancel := context.WithCancel(context.Background())
	pf := &Platform{cfg: cfg, manager: mgr, orch: orch, pool: p, mem: mem, cancel: cancel}

	// deployment 起動(native は §8.7 検証つき load)
	applyCtx, applyCancel := context.WithTimeout(ctx, 60*time.Second)
	mgr.Apply(applyCtx)
	applyCancel()

	go mgr.StartHealth(ctx, time.Duration(cfg.Pool.HealthIntervalSeconds)*time.Second)
	if mem != nil {
		go pf.retentionLoop(ctx)
	}

	// Workflow Orchestrator(MRCI-002、opt-in)。Step 実行は本 Platform の
	// Execute/Cancel を再利用する。
	if cfg.Workflows.Enabled {
		ws, err := workflow.Open(cfg, catalogResolver{cfg}, pf)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("platform: workflow store: %w", err)
		}
		pf.workflows = ws
	}
	return pf, nil
}

// catalogResolver answers workflow validation questions from the config。
type catalogResolver struct{ cfg *platformcfg.Config }

func (r catalogResolver) ModelExists(name string) bool {
	m := r.cfg.FindModel(name)
	return m != nil && m.ApprovalStatus == platformcfg.ApprovalApproved
}

func (r catalogResolver) PoolExists(id string) bool { return r.cfg.FindPool(id) != nil }

// Close stops background loops。
func (p *Platform) Close() {
	if p.workflows != nil {
		p.workflows.Close()
	}
	p.cancel()
}

// retentionLoop periodically enforces physical deletion (§11.4)。
func (p *Platform) retentionLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := p.mem.SweepExpired()
			if err != nil {
				slog.Error("retention sweep failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("retention sweep completed", "deleted", n)
			}
		}
	}
}

// --- contract.Backend ---

func (p *Platform) GetPlatformCapabilities(ctx context.Context) contract.Capabilities {
	return contract.Capabilities{
		ContractVersion: contract.Version,
		Streaming:       true,
		Conversation:    p.mem != nil,
		NativeEngine:    p.manager.HasNativeEngine(),
		DistributedPool: true,
		// Task decomposition(§7 Phase 6)は未実装 — 未実装能力を実装済みと
		// して公開しない(§8.3)。
		TaskDecomposition: false,
	}
}

func (p *Platform) Execute(ctx context.Context, req *contract.Request) (*contract.Response, *contract.Error) {
	return p.orch.Execute(ctx, req)
}

func (p *Platform) ExecuteStream(ctx context.Context, req *contract.Request, w contract.StreamWriter) (*contract.StreamResult, *contract.Error) {
	return p.orch.ExecuteStream(ctx, req, w)
}

func (p *Platform) Cancel(ctx context.Context, requestID string) { p.orch.Cancel(requestID) }

func (p *Platform) CountTokens(ctx context.Context, req *contract.Request) (int64, *contract.Error) {
	return p.orch.CountTokens(ctx, req)
}

func (p *Platform) GetRouteStatus(ctx context.Context, logicalModel string) (*contract.RouteStatus, *contract.Error) {
	return p.manager.RouteStatus(logicalModel)
}

func (p *Platform) ListModels(ctx context.Context) []contract.ModelInfo {
	return p.manager.Models()
}

// Memory exposes the conversation store for管理操作(削除等)。stateless 時 nil。
func (p *Platform) Memory() *memory.Store { return p.mem }

// Manager exposes deployment管理(drain/resume)— Model Manager 経由のみ
// (Gateway へは公開しない)。
func (p *Platform) Manager() *modelmanager.Manager { return p.manager }

// Nodes returns the pool snapshot(本文なし)。
func (p *Platform) Nodes() []pool.Node { return p.pool.Snapshot() }

// Workflows exposes the Workflow Orchestrator(無効時 nil)。
func (p *Platform) Workflows() *workflow.Service { return p.workflows }
