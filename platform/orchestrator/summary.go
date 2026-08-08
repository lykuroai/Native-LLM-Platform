package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
)

// scheduleSummary launches an async summary job (§10.3 手順9)。
// summary_model 未設定時は何もしない(古い message は文脈から外れるのみ —
// 原文は retention まで保持され、勝手に削除しない §18)。
func (o *Orchestrator) scheduleSummary(convID string, fromSeq, toSeq int64, orig *contract.Request) {
	model := o.cfg.Memory.SummaryModel
	if model == "" || o.mem == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		conv, err := o.mem.GetOrCreate(convID, orig.Policy.DataClass, 0)
		if err != nil {
			return
		}
		var text string
		for _, m := range conv.Messages {
			if m.Seq >= fromSeq && m.Seq <= toSeq && m.Status == "complete" {
				text += m.Role + ": " + m.Content + "\n"
			}
		}
		if text == "" {
			return
		}
		prompt := "Summarize the following conversation compactly, preserving decisions and facts:\n\n" + text
		msgs, _ := json.Marshal([]map[string]string{{"role": "user", "content": prompt}})
		req := &contract.Request{
			ContractVersion: contract.Version,
			RequestID:       orig.RequestID + "-sum",
			TenantScope:     orig.TenantScope,
			ActorScope:      "platform-summary",
			Policy:          orig.Policy,
			LogicalModel:    model,
			Endpoint:        "/v1/chat/completions",
			NormalizedInput: map[string]json.RawMessage{"messages": msgs},
		}
		resp, cerr := o.Execute(ctx, req)
		if cerr != nil || resp.StatusCode != 200 {
			slog.Warn("conversation summary generation failed", "conversation", convID)
			return // 原文は保持されたまま — 次回再試行(§18: retry)
		}
		summary := assistantFromBody(resp.Body)
		if summary == "" {
			return
		}
		if err := o.mem.ReplaceWithSummary(convID, fromSeq, toSeq, summary, model); err != nil {
			slog.Warn("conversation summary store failed", "conversation", convID, "error", err)
		}
	}()
}
