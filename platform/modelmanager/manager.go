// Package modelmanager owns the model catalog and deployment lifecycle (§6)。
//
// Engine Control API クライアントは本パッケージのみが保持する(§8.2 /
// AT-G03)— Orchestrator へは Data API だけを引き渡し、Gateway はどちらも
// 保持しない。model の load は §8.7 の検証順(承認確認 → digest → load →
// available 切替)に従い、best effort load をしない。
package modelmanager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/connector"
	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/enginecontract"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
	"github.com/lykuroai/Native-LLM-Platform/platform/pool"
)

// EnginePair bundles one engine's API clients。Control は Manager 専有。
type EnginePair struct {
	Data    enginecontract.DataAPI
	Control enginecontract.ControlAPI
}

// Candidate is one routable deployment for a logical model。
type Candidate struct {
	DeploymentID  string
	RouteType     string // native | connector
	ContextLength int
	// connector 経路
	Connector     *connector.OpenAICompatible
	PhysicalModel string
	// native 経路
	Engine          enginecontract.DataAPI
	ModelInstanceID string
}

// Manager resolves logical models to ready deployments。
type Manager struct {
	mu         sync.RWMutex
	cfg        *platformcfg.Config
	connectors map[string]*connector.OpenAICompatible // runtime_endpoint_id -> connector
	engines    map[string]EnginePair                  // engine_id -> clients
	depStatus  map[string]string                      // deployment_id -> available|unavailable|suspended
	pool       *pool.Pool
}

// New wires the manager。engines には接続済み Engine のみを渡す(実 Engine は
// 実装中のため、通常は "mock:" endpoint の MockEngine か空)。
func New(cfg *platformcfg.Config, p *pool.Pool, engines map[string]EnginePair) *Manager {
	m := &Manager{
		cfg:        cfg,
		connectors: map[string]*connector.OpenAICompatible{},
		engines:    engines,
		depStatus:  map[string]string{},
		pool:       p,
	}
	if engines == nil {
		m.engines = map[string]EnginePair{}
	}
	for _, ep := range cfg.RuntimeEndpoints {
		m.connectors[ep.ID] = connector.New(ep)
	}
	return m
}

// Apply performs deployment activation for the current config。
// native deployment は §8.7 の検証つき load を行い、失敗時は available へ
// 切り替えない(直前状態維持は呼出側の config 世代管理が担う)。
func (m *Manager) Apply(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mdl := range m.cfg.Models {
		for _, dep := range mdl.Deployments {
			key := dep.ID
			if mdl.ApprovalStatus != platformcfg.ApprovalApproved {
				// 承認されていない model の deployment は routing 対象外(§6.6)
				m.depStatus[key] = "suspended"
				continue
			}
			switch dep.BackendType {
			case platformcfg.BackendExternalConnector:
				m.depStatus[key] = "available" // 実疎通は health probe が降格させる
				m.pool.Register(key, "connector")
			case platformcfg.BackendNativeEngine:
				pair, ok := m.engines[dep.EngineID]
				if !ok {
					// Engine 未接続(実装中)— 未実装能力を実装済みとして
					// 公開しない(§8.3)
					m.depStatus[key] = "unavailable"
					continue
				}
				if err := pair.Control.LoadModel(ctx, &enginecontract.LoadModelRequest{
					ModelInstanceID: dep.ModelInstanceID,
					ArtifactPath:    dep.ArtifactPath,
					ArtifactDigest:  dep.ArtifactDigest,
				}); err != nil {
					slog.Error("native model load failed",
						"deployment", dep.ID, "code", err.Code)
					m.depStatus[key] = "unavailable"
					continue
				}
				st, serr := pair.Control.GetModelStatus(ctx, dep.ModelInstanceID)
				if serr != nil || st.State != "ready" {
					m.depStatus[key] = "unavailable"
					continue
				}
				m.depStatus[key] = "available"
				m.pool.Register(key, "native")
			}
		}
	}
}

// Drain stops new work on a deployment (native は Engine Drain も実行)。
func (m *Manager) Drain(ctx context.Context, deploymentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dep := m.findDeployment(deploymentID)
	if dep == nil {
		return fmt.Errorf("unknown deployment %q", deploymentID)
	}
	m.pool.Drain(deploymentID)
	if dep.BackendType == platformcfg.BackendNativeEngine {
		if pair, ok := m.engines[dep.EngineID]; ok {
			if err := pair.Control.Drain(ctx); err != nil {
				return fmt.Errorf("engine drain: %s", err.Code)
			}
		}
	}
	return nil
}

// Resume re-enables a drained deployment。
func (m *Manager) Resume(ctx context.Context, deploymentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dep := m.findDeployment(deploymentID)
	if dep == nil {
		return fmt.Errorf("unknown deployment %q", deploymentID)
	}
	if dep.BackendType == platformcfg.BackendNativeEngine {
		if pair, ok := m.engines[dep.EngineID]; ok {
			if err := pair.Control.Resume(ctx); err != nil {
				return fmt.Errorf("engine resume: %s", err.Code)
			}
		}
	}
	m.pool.Resume(deploymentID)
	return nil
}

func (m *Manager) findDeployment(id string) *platformcfg.Deployment {
	for i := range m.cfg.Models {
		for j := range m.cfg.Models[i].Deployments {
			if m.cfg.Models[i].Deployments[j].ID == id {
				return &m.cfg.Models[i].Deployments[j]
			}
		}
	}
	return nil
}

// Resolve returns routable candidates for a logical model under the given
// policy (§12.9: Router は Gateway の許可範囲内で選択し、上位 Policy を
// 緩和しない)。
func (m *Manager) Resolve(logical string, policy contract.PolicyContext) ([]Candidate, *contract.Error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry := m.cfg.FindModel(logical)
	if entry == nil {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable,
			Message: "unknown model " + logical}
	}
	if entry.ApprovalStatus != platformcfg.ApprovalApproved {
		return nil, &contract.Error{Code: contract.ErrModelNotAllowed,
			Message: "model is not approved for use"}
	}
	// model 単位の data class 制限(Gateway 許可の内側でさらに絞る)
	if len(entry.AllowedDataClasses) > 0 && policy.DataClass != "" {
		ok := false
		for _, dc := range entry.AllowedDataClasses {
			if dc == policy.DataClass {
				ok = true
				break
			}
		}
		if !ok {
			return nil, &contract.Error{Code: contract.ErrModelNotAllowed,
				Message: "declared data class is not allowed for this model"}
		}
	}
	var out []Candidate
	for _, dep := range entry.Deployments {
		if m.depStatus[dep.ID] != "available" {
			continue
		}
		c := Candidate{
			DeploymentID:  dep.ID,
			ContextLength: entry.Capabilities.ContextLength,
		}
		switch dep.BackendType {
		case platformcfg.BackendExternalConnector:
			conn, ok := m.connectors[dep.RuntimeEndpointID]
			if !ok {
				continue
			}
			c.RouteType = "connector"
			c.Connector = conn
			c.PhysicalModel = dep.PhysicalModel
		case platformcfg.BackendNativeEngine:
			pair, ok := m.engines[dep.EngineID]
			if !ok {
				continue
			}
			c.RouteType = "native"
			c.Engine = pair.Data
			c.ModelInstanceID = dep.ModelInstanceID
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable,
			Message: "no deployment is available for model " + logical, RetryAfter: 10}
	}
	return out, nil
}

// Models lists logical models with readiness (物理情報非開示)。
func (m *Manager) Models() []contract.ModelInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]contract.ModelInfo, 0, len(m.cfg.Models))
	for _, mdl := range m.cfg.Models {
		ready := false
		if mdl.ApprovalStatus == platformcfg.ApprovalApproved {
			for _, dep := range mdl.Deployments {
				if m.depStatus[dep.ID] == "available" {
					ready = true
					break
				}
			}
		}
		out = append(out, contract.ModelInfo{LogicalName: mdl.LogicalName, Ready: ready})
	}
	return out
}

// RouteStatus reports deployment readiness for one logical model (§5.3)。
func (m *Manager) RouteStatus(logical string) (*contract.RouteStatus, *contract.Error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry := m.cfg.FindModel(logical)
	if entry == nil {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "unknown model"}
	}
	rs := &contract.RouteStatus{LogicalModel: logical}
	for _, dep := range entry.Deployments {
		status := m.depStatus[dep.ID]
		if status == "" {
			status = "unavailable"
		}
		routeType := "connector"
		if dep.BackendType == platformcfg.BackendNativeEngine {
			routeType = "native"
		}
		if status == "available" {
			rs.Ready = true
		}
		rs.Deployments = append(rs.Deployments, contract.DeploymentStatus{
			ID: dep.ID, RouteType: routeType, Status: status,
		})
	}
	return rs, nil
}

// HasNativeEngine reports whether any engine is connected。
func (m *Manager) HasNativeEngine() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.engines) > 0
}

// StartHealth runs periodic health probes until ctx is done (§9.3)。
func (m *Manager) StartHealth(ctx context.Context, interval time.Duration) {
	probe := func() {
		m.mu.RLock()
		type target struct {
			depID string
			conn  *connector.OpenAICompatible
			eng   *EnginePair
			inst  string
		}
		var targets []target
		for _, mdl := range m.cfg.Models {
			for _, dep := range mdl.Deployments {
				if m.depStatus[dep.ID] == "suspended" || m.depStatus[dep.ID] == "unavailable" {
					continue
				}
				t := target{depID: dep.ID}
				if dep.BackendType == platformcfg.BackendExternalConnector {
					t.conn = m.connectors[dep.RuntimeEndpointID]
				} else if pair, ok := m.engines[dep.EngineID]; ok {
					t.eng = &pair
					t.inst = dep.ModelInstanceID
				}
				targets = append(targets, t)
			}
		}
		m.mu.RUnlock()
		for _, t := range targets {
			healthy := false
			switch {
			case t.conn != nil:
				healthy = t.conn.Health(ctx)
			case t.eng != nil:
				if st, err := t.eng.Control.GetModelStatus(ctx, t.inst); err == nil {
					healthy = st.State == "ready"
				}
			}
			m.pool.SetHealth(t.depID, healthy)
		}
	}
	probe()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe()
		}
	}
}
