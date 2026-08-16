package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

type fakeExec struct {
	mu      sync.Mutex
	calls   []*contract.Request
	handler func(call int, req *contract.Request) (*contract.Response, *contract.Error)
	block   chan struct{} // non-nil: Execute waits (cancel テスト用)
}

func (f *fakeExec) Execute(ctx context.Context, req *contract.Request) (*contract.Response, *contract.Error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	n := len(f.calls)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, &contract.Error{Code: contract.ErrInferenceTimeout, Message: "cancelled"}
		}
	}
	return f.handler(n, req)
}

func (f *fakeExec) Cancel(ctx context.Context, requestID string) {}

func (f *fakeExec) nCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func chatResp(content string, in, out int64) *contract.Response {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}}},
	})
	return &contract.Response{StatusCode: 200, ContentType: "application/json", Body: body,
		Usage: contract.Usage{InputTokens: &in, OutputTokens: &out}, DeploymentID: "dep-1", RouteType: "connector"}
}

func newService(t *testing.T, exec Executor) *Service {
	t.Helper()
	cfg := &platformcfg.Config{Workflows: platformcfg.Workflows{
		Enabled: true, DataDir: t.TempDir(), MaxFlows: 10,
		RunRetentionDays: 90, EventRetentionDays: 30, MaxConcurrentRuns: 4,
	}}
	s, err := Open(cfg, testResolver, exec)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func publishFlow(t *testing.T, s *Service, mutate func(m map[string]any)) *FlowMeta {
	t.Helper()
	meta, errs := s.CreateFlow(validDef(t, mutate))
	require.Empty(t, errs)
	_, errs = s.PublishFlow(meta.FlowID, 1)
	require.Empty(t, errs)
	return meta
}

func waitRun(t *testing.T, run *Run) {
	t.Helper()
	select {
	case <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
}

func startParams() StartParams {
	return StartParams{Alias: "two-runtime-follow-up",
		Inputs:       map[string]string{"question1": "Q1", "question2": "Q2"},
		ResponseMode: "sync", KeyID: "vk1", RequestID: "req_test"}
}

// IT-01 相当: A の出力と質問2が B へ正しい順序で渡り、最終出力が返る。
func TestChainHandoff(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		if req.LogicalModel == "model-a" {
			return chatResp("ANSWER-A", 10, 20), nil
		}
		return chatResp("FINAL-B", 30, 40), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	snap, err := s.Runs().Snapshot(run.ID)
	require.NoError(t, err)
	require.Equal(t, RunCompleted, snap.Status)
	require.Equal(t, int64(40), snap.InputTokens)
	require.Equal(t, int64(60), snap.OutputTokens)
	require.Equal(t, 1, snap.FlowVersion)
	require.NotEmpty(t, snap.FlowChecksum)

	out, ok := s.Runs().Output(run.ID)
	require.True(t, ok)
	require.Equal(t, "FINAL-B", out)

	// Handoff: B への入力に Q1・回答A・Q2 がこの順で含まれる
	require.Equal(t, 2, exec.nCalls())
	reqB := exec.calls[1]
	var msgs []map[string]string
	require.NoError(t, json.Unmarshal(reqB.NormalizedInput["messages"], &msgs))
	user := msgs[len(msgs)-1]["content"]
	iQ1, iA, iQ2 := strings.Index(user, "Q1"), strings.Index(user, "ANSWER-A"), strings.Index(user, "Q2")
	require.True(t, iQ1 >= 0 && iA > iQ1 && iQ2 > iA, "handoff order broken: %q", user)

	// Step Attempt = contract request、RequestID 形式・pool 指定
	require.Equal(t, run.ID+"-runtime_a-a1", exec.calls[0].RequestID)
	require.Equal(t, "pool-1", reqB.PoolID)
}

// IT-05 相当: 一時障害後に再試行し、Attempt が記録される。
func TestRetryOnTransientError(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		if n == 1 {
			return nil, &contract.Error{Code: contract.ErrCapacityExhausted, Message: "busy"}
		}
		return chatResp("ok", 1, 1), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, RunCompleted, snap.Status)
	require.Equal(t, 2, snap.Steps[0].Attempts)
	require.Len(t, snap.Steps[0].Decisions, 2)
	require.Equal(t, contract.ErrCapacityExhausted, snap.Steps[0].Decisions[0].ErrorCode)
}

// IT-06 相当: Primary Pool 候補なし → 承認済み Fallback Pool へ切替。
func TestFallbackPool(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		if req.LogicalModel == "model-a" {
			return chatResp("a", 1, 1), nil
		}
		if req.PoolID == "pool-1" {
			return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "no candidate in pool"}
		}
		return chatResp("via-fallback", 1, 1), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, RunCompleted, snap.Status)
	decs := snap.Steps[1].Decisions
	last := decs[len(decs)-1]
	require.True(t, last.FallbackUsed)
	require.Equal(t, "pool-safe", last.PoolID)
	out, _ := s.Runs().Output(run.ID)
	require.Equal(t, "via-fallback", out)
}

// IT-07 相当: 全候補・全 Fallback 不可 → Fail Closed(no_eligible_runtime)。
func TestFailClosedWhenNoEligibleRuntime(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "none"}
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, RunFailed, snap.Status)
	require.Equal(t, CodeNoEligible, snap.ErrorCode)
	require.Equal(t, StepFailed, snap.Steps[0].Status)
	require.Equal(t, StepSkipped, snap.Steps[1].Status)
}

// タイムアウトは再試行・Fallback しない(二重推論防止の既存原則)。
func TestTimeoutDoesNotFailover(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return nil, &contract.Error{Code: contract.ErrInferenceTimeout, Message: "deadline"}
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, RunFailed, snap.Status)
	require.Equal(t, CodeStepTimeout, snap.ErrorCode)
	require.Equal(t, StepTimedOut, snap.Steps[0].Status)
	require.Equal(t, 1, exec.nCalls(), "timeout must not retry or fall back")
}

// Budget: max_total_tokens 超過で後続 Step を実行しない。
func TestBudgetExceeded(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("big", 600, 600), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, func(m map[string]any) {
		m["limits"] = map[string]any{"max_total_tokens": 1000}
	})

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, RunFailed, snap.Status)
	require.Equal(t, CodeBudgetExceeded, snap.ErrorCode)
	require.Equal(t, 1, exec.nCalls())
	require.Equal(t, StepSkipped, snap.Steps[1].Status)
}

// IT-11 相当: Cancel が実行中 Step へ伝播する。
func TestCancelRun(t *testing.T) {
	block := make(chan struct{})
	exec := &fakeExec{block: block, handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("late", 1, 1), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	require.Eventually(t, func() bool { return exec.nCalls() >= 1 }, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, s.CancelRun(run.ID))
	waitRun(t, run)
	close(block)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, RunCancelled, snap.Status)
	// 冪等
	require.NoError(t, s.CancelRun(run.ID))
}

// IT-09 相当: Idempotency-Key の重複実行防止。
func TestIdempotency(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("ok", 1, 1), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	p := startParams()
	p.IdemKey = "key-1"
	run1, rerr := s.StartRun(p)
	require.Nil(t, rerr)
	waitRun(t, run1)

	run2, rerr := s.StartRun(p)
	require.Nil(t, rerr)
	require.Equal(t, run1.ID, run2.ID)

	p2 := p
	p2.Inputs = map[string]string{"question1": "different", "question2": "Q2"}
	_, rerr = s.StartRun(p2)
	require.NotNil(t, rerr)
	require.Equal(t, CodeIdemConflict, rerr.Code)
}

// IT-10 相当: Draft 更新は実行済み Run が固定した Version に影響しない。
func TestRunPinsVersion(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("v1-output", 1, 1), nil
	}}
	s := newService(t, exec)
	meta := publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	// Draft を更新して v2 を公開しても、既存 Run は v1 のまま
	_, errs := s.UpdateFlow(meta.FlowID, 1, validDef(t, func(m map[string]any) { m["name"] = "changed" }))
	require.Empty(t, errs)
	_, errs = s.PublishFlow(meta.FlowID, 2)
	require.Empty(t, errs)

	snap, _ := s.Runs().Snapshot(run.ID)
	require.Equal(t, 1, snap.FlowVersion)

	run2, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run2)
	snap2, _ := s.Runs().Snapshot(run2.ID)
	require.Equal(t, 2, snap2.FlowVersion)
}

// 実行系エラー: 入力 Validation・未公開 Flow・容量。
func TestStartRunErrors(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("ok", 1, 1), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	p := startParams()
	p.Alias = "nope"
	_, rerr := s.StartRun(p)
	require.Equal(t, CodeNotFound, rerr.Code)

	p = startParams()
	p.FlowVersion = 99
	_, rerr = s.StartRun(p)
	require.Equal(t, CodeNotPublished, rerr.Code)

	p = startParams()
	p.Inputs = map[string]string{"question1": "only"}
	_, rerr = s.StartRun(p)
	require.Equal(t, CodeInputValidation, rerr.Code)
}

// SSE 再開: Last-Event-ID 以降の Event を再取得できる(IT-08 相当)。
func TestEventSubscribeReplay(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("ok", 1, 1), nil
	}}
	s := newService(t, exec)
	publishFlow(t, s, nil)

	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	waitRun(t, run)

	replay, _, cancel, err := s.Runs().Subscribe(run.ID, 0)
	require.NoError(t, err)
	defer cancel()
	require.NotEmpty(t, replay)
	var types []string
	var lastSeq int64
	for _, ev := range replay {
		require.Greater(t, ev.Seq, lastSeq, "sequence must be monotonic")
		lastSeq = ev.Seq
		types = append(types, ev.Type)
		// 永続 Event に本文が含まれない
		raw, _ := json.Marshal(ev.Data)
		require.NotContains(t, string(raw), "FINAL")
		require.NotContains(t, string(raw), "Q1")
	}
	joined := strings.Join(types, ",")
	require.Contains(t, joined, "workflow.queued")
	require.Contains(t, joined, "workflow.started")
	require.Contains(t, joined, "step.started")
	require.Contains(t, joined, "step.completed")
	require.Contains(t, joined, "workflow.completed")

	// 途中 seq からの再開
	mid := replay[2].Seq
	replay2, _, cancel2, err := s.Runs().Subscribe(run.ID, mid)
	require.NoError(t, err)
	defer cancel2()
	require.Equal(t, len(replay)-3, len(replay2))
}

// 再起動 Recovery: 実行中 Run は FAILED(interrupted) へ確定(縮退版 IT-12)。
func TestRestartMarksRunningAsInterrupted(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("ok", 1, 1), nil
	}}
	cfg := &platformcfg.Config{Workflows: platformcfg.Workflows{
		Enabled: true, DataDir: t.TempDir(), MaxFlows: 10,
		RunRetentionDays: 90, EventRetentionDays: 30, MaxConcurrentRuns: 4,
	}}
	s, err := Open(cfg, testResolver, exec)
	require.NoError(t, err)
	publishFlow(t, s, nil)

	block := make(chan struct{})
	exec.block = block
	run, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	require.Eventually(t, func() bool { return exec.nCalls() >= 1 }, 5*time.Second, 5*time.Millisecond)
	s.Close()

	// 別プロセス相当で再オープン
	s2, err := Open(cfg, testResolver, exec)
	require.NoError(t, err)
	defer s2.Close()

	snap, err := s2.Runs().Snapshot(run.ID)
	require.NoError(t, err)
	require.Equal(t, RunFailed, snap.Status)
	require.Equal(t, "interrupted", snap.ErrorCode)

	// 旧プロセス相当の goroutine を終了させてから TempDir を掃除する
	close(block)
	waitRun(t, run)
}

// Run メタデータのディスク上に本文が残らない(IT-15 相当)。
func TestNoContentOnDisk(t *testing.T) {
	exec := &fakeExec{handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("SECRET-OUTPUT-XYZ", 1, 1), nil
	}}
	cfg := &platformcfg.Config{Workflows: platformcfg.Workflows{
		Enabled: true, DataDir: t.TempDir(), MaxFlows: 10,
		RunRetentionDays: 90, EventRetentionDays: 30, MaxConcurrentRuns: 4,
	}}
	s, err := Open(cfg, testResolver, exec)
	require.NoError(t, err)
	defer s.Close()
	publishFlow(t, s, nil)

	p := startParams()
	p.Inputs["question1"] = "SECRET-INPUT-ABC"
	run, rerr := s.StartRun(p)
	require.Nil(t, rerr)
	waitRun(t, run)

	var hits []string
	root := cfg.Workflows.DataDir + "/runs"
	err = filepathWalk(root, func(path string, data []byte) {
		if strings.Contains(string(data), "SECRET-INPUT-ABC") || strings.Contains(string(data), "SECRET-OUTPUT-XYZ") {
			hits = append(hits, path)
		}
	})
	require.NoError(t, err)
	require.Empty(t, hits, "run files must not contain prompt/response content")
}

func filepathWalk(root string, fn func(path string, data []byte)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fn(path, data)
		return nil
	})
}

// 容量: MaxConcurrentRuns 超過は即時拒否。
func TestRunCapacity(t *testing.T) {
	block := make(chan struct{})
	exec := &fakeExec{block: block, handler: func(n int, req *contract.Request) (*contract.Response, *contract.Error) {
		return chatResp("ok", 1, 1), nil
	}}
	cfg := &platformcfg.Config{Workflows: platformcfg.Workflows{
		Enabled: true, DataDir: t.TempDir(), MaxFlows: 10,
		RunRetentionDays: 90, EventRetentionDays: 30, MaxConcurrentRuns: 1,
	}}
	s, err := Open(cfg, testResolver, exec)
	require.NoError(t, err)
	defer s.Close()
	publishFlow(t, s, nil)

	run1, rerr := s.StartRun(startParams())
	require.Nil(t, rerr)
	require.NotNil(t, run1)

	_, rerr = s.StartRun(startParams())
	require.NotNil(t, rerr)
	require.Equal(t, CodeCapacity, rerr.Code)
	require.Equal(t, fmt.Sprint(10), fmt.Sprint(rerr.RetryAfter))

	close(block)
	waitRun(t, run1)
}
