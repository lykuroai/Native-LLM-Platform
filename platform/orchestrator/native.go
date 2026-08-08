package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/enginecontract"
	"github.com/lykuroai/Native-LLM-Platform/platform/modelmanager"
)

// native.go bridges the OpenAI-compatible外部形式と platform-engine-v1。
// Gateway 外部 contract は OpenAI 互換のまま(§13.1)、Engine 内部形式は
// 外部へ漏らさない(§0.4: Runtime固有情報を外部APIへ漏らさない)。

// engineInputFromBody converts OpenAI messages/prompt to engine Input。
func engineInputFromBody(body map[string]json.RawMessage) (enginecontract.Input, *contract.Error) {
	var input enginecontract.Input
	if raw, ok := body["messages"]; ok {
		var msgs []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return input, &contract.Error{Code: contract.ErrInvalidRequest, Message: "messages must be an array"}
		}
		for _, m := range msgs {
			var text string
			if err := json.Unmarshal(m.Content, &text); err != nil {
				// マルチモーダル content は Engine MVP 対象外(§2.1: text のみ)
				return input, &contract.Error{Code: contract.ErrInvalidRequest,
					Message: "only text content is supported on this deployment"}
			}
			input.Messages = append(input.Messages, enginecontract.Message{Role: m.Role, Content: text})
		}
		return input, nil
	}
	if raw, ok := body["prompt"]; ok {
		if err := json.Unmarshal(raw, &input.Prompt); err != nil {
			return input, &contract.Error{Code: contract.ErrInvalidRequest, Message: "prompt must be a string"}
		}
		return input, nil
	}
	return input, &contract.Error{Code: contract.ErrInvalidRequest, Message: "messages or prompt is required"}
}

// engineRequest builds a platform-engine-v1 GenerateRequest from the
// normalized OpenAI body。
func engineRequest(cand modelmanager.Candidate, req *contract.Request, body map[string]json.RawMessage, deadline time.Time) (*enginecontract.GenerateRequest, *contract.Error) {
	input, cerr := engineInputFromBody(body)
	if cerr != nil {
		return nil, cerr
	}
	gen := enginecontract.Generation{}
	if raw, ok := body["max_tokens"]; ok {
		_ = json.Unmarshal(raw, &gen.MaxOutputTokens)
	}
	if raw, ok := body["max_completion_tokens"]; ok {
		_ = json.Unmarshal(raw, &gen.MaxOutputTokens)
	}
	if raw, ok := body["temperature"]; ok {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			gen.Temperature = &v
		}
	}
	if raw, ok := body["top_p"]; ok {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			gen.TopP = &v
		}
	}
	if raw, ok := body["seed"]; ok {
		var v int64
		if json.Unmarshal(raw, &v) == nil {
			gen.Seed = &v
		}
	}
	if raw, ok := body["stop"]; ok {
		var single string
		if json.Unmarshal(raw, &single) == nil {
			gen.Stop = []string{single}
		} else {
			_ = json.Unmarshal(raw, &gen.Stop)
		}
	}
	return &enginecontract.GenerateRequest{
		RequestID:       req.RequestID,
		TraceID:         req.TraceID,
		TenantScope:     req.TenantScope,
		ProjectScope:    req.ProjectScope,
		ModelInstanceID: cand.ModelInstanceID,
		Input:           input,
		Generation:      gen,
		Scheduling:      enginecontract.Scheduling{DeadlineUnixMS: deadline.UnixMilli()},
	}, nil
}

func engineErrToContract(err *enginecontract.EngineError) *contract.Error {
	return &contract.Error{
		Code:    enginecontract.PlatformCode(err.Code),
		Message: "engine: " + err.Message,
	}
}

// openAICompletion renders an Engine response as an OpenAI chat completion。
func openAICompletion(req *contract.Request, resp *enginecontract.GenerateResponse) []byte {
	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{
		"id":      "chatcmpl-" + resp.RequestID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.LogicalModel, // 物理instance IDは外部へ返さない
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": resp.OutputText},
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
	}
	b, _ := json.Marshal(out)
	return b
}

// nativeExecute runs one non-streaming request on the Native Engine。
func (o *Orchestrator) nativeExecute(ctx context.Context, cand modelmanager.Candidate, req *contract.Request, p *prepared) (*contract.Response, *contract.Error) {
	dl, _ := ctx.Deadline()
	ereq, cerr := engineRequest(cand, req, p.body, dl)
	if cerr != nil {
		return nil, cerr
	}
	resp, eerr := cand.Engine.Generate(ctx, ereq)
	if eerr != nil {
		if eerr.Code == enginecontract.CodeRequestCancelled {
			return nil, &contract.Error{Code: contract.ErrInferenceTimeout, Message: "request cancelled"}
		}
		return nil, engineErrToContract(eerr)
	}
	in, out := resp.Usage.InputTokens, resp.Usage.OutputTokens
	return &contract.Response{
		StatusCode:   200,
		ContentType:  "application/json; charset=utf-8",
		Body:         openAICompletion(req, resp),
		Usage:        contract.Usage{InputTokens: &in, OutputTokens: &out},
		DeploymentID: cand.DeploymentID,
		RouteType:    "native",
	}, nil
}

// nativeStream runs a streaming request, rendering engine events as
// OpenAI-compatible SSE chunks(event 順序は §9.7 を SSE へ写像)。
func (o *Orchestrator) nativeStream(ctx context.Context, cand modelmanager.Candidate, req *contract.Request, p *prepared, w contract.StreamWriter) (*contract.StreamResult, *contract.Error) {
	dl, _ := ctx.Deadline()
	ereq, cerr := engineRequest(cand, req, p.body, dl)
	if cerr != nil {
		return nil, cerr
	}
	result := &contract.StreamResult{}
	started := false
	chunkID := "chatcmpl-" + req.RequestID
	writeEvent := func(payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if !w.Chunk([]byte("data: " + string(b) + "\n")) {
			return false
		}
		ok := w.Chunk([]byte("\n"))
		w.Flush()
		return ok
	}
	eerr := cand.Engine.GenerateStream(ctx, ereq, func(ev enginecontract.StreamEvent) bool {
		switch ev.Type {
		case enginecontract.EventStarted:
			if !started {
				w.Start(200, "text/event-stream")
				started = true
			}
			return true
		case enginecontract.EventTextDelta:
			return writeEvent(map[string]any{
				"id": chunkID, "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": req.LogicalModel,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{"content": ev.Delta},
				}},
			})
		case enginecontract.EventUsage:
			if ev.Usage != nil {
				in, out := ev.Usage.InputTokens, ev.Usage.OutputTokens
				result.Usage = contract.Usage{InputTokens: &in, OutputTokens: &out}
			}
			return true
		case enginecontract.EventCompleted:
			finish := "stop"
			if ev.Final != nil && ev.Final.FinishReason != "" {
				finish = ev.Final.FinishReason
			}
			usage := map[string]any{}
			if ev.Final != nil {
				usage = map[string]any{
					"prompt_tokens":     ev.Final.Usage.InputTokens,
					"completion_tokens": ev.Final.Usage.OutputTokens,
					"total_tokens":      ev.Final.Usage.TotalTokens,
				}
				in, out := ev.Final.Usage.InputTokens, ev.Final.Usage.OutputTokens
				result.Usage = contract.Usage{InputTokens: &in, OutputTokens: &out}
			}
			if !writeEvent(map[string]any{
				"id": chunkID, "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": req.LogicalModel,
				"choices": []map[string]any{{
					"index": 0, "delta": map[string]any{}, "finish_reason": finish,
				}},
				"usage": usage,
			}) {
				return false
			}
			ok := w.Chunk([]byte("data: [DONE]\n"))
			w.Chunk([]byte("\n"))
			w.Flush()
			return ok
		case enginecontract.EventError:
			// stream 開始後の失敗は SSE error event で終了(§13.1.1)
			msg := "inference failed"
			if ev.Err != nil {
				msg = fmt.Sprintf("engine error: %s", ev.Err.Code)
			}
			writeEvent(map[string]any{
				"error": map[string]any{
					"message": msg, "type": "service_error",
					"code": "model_unavailable", "request_id": req.RequestID,
				},
			})
			return false
		}
		return true
	})
	if eerr != nil {
		if eerr.Code == enginecontract.CodeRequestCancelled {
			result.ClientGone = true
			return result, nil
		}
		if started {
			// 既に SSE 開始済み — error event を流して正常終了扱い
			writeEvent(map[string]any{
				"error": map[string]any{
					"message": "engine error: " + eerr.Code, "type": "service_error",
					"code": "model_unavailable", "request_id": req.RequestID,
				},
			})
			result.ClientGone = false
			return result, nil
		}
		return nil, engineErrToContract(eerr)
	}
	return result, nil
}
