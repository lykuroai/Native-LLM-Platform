package memory

import "encoding/json"

// BuiltContext is the Context Builder output (§10.4)。
type BuiltContext struct {
	// Messages is the merged history in OpenAI chat format(JSON化済み)。
	Messages []json.RawMessage
	// TokenEst is the estimated input tokens of the merged history。
	TokenEst int64
	// NeedsSummary reports that older messages exceeded the budget and a
	// summary job should run (§10.5)。
	NeedsSummary bool
	// SummaryFromSeq/ToSeq is the suggested summarization range。
	SummaryFromSeq, SummaryToSeq int64
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// BuildContext merges stored history with the current request messages
// (§10.4 構成順・優先順位)。
//
//   - current(system 含む)は truncate しない(§10.5)
//   - summary → 過去 message(新しい順に budget まで)→ current の順で組む
//   - budget = contextWindow - reservedOutput - safetyMargin
//
// currentMsgs はクライアントが今回送った messages 配列(生 JSON)。
func BuildContext(c *Conversation, currentMsgs []json.RawMessage, contextWindow, reservedOutput int) *BuiltContext {
	const safetyMargin = 256
	if contextWindow <= 0 {
		contextWindow = 8192
	}
	if reservedOutput <= 0 {
		reservedOutput = 1024
	}
	budget := int64(contextWindow - reservedOutput - safetyMargin)

	out := &BuiltContext{}

	// current 分の消費を先に確保(truncate 対象にしない)
	var currentTokens int64
	for _, raw := range currentMsgs {
		var m chatMessage
		if json.Unmarshal(raw, &m) == nil {
			currentTokens += estimateTokens(m.Content)
		}
	}
	remain := budget - currentTokens

	// summary(古い範囲の要約)を先頭側に置く
	prefix := make([]json.RawMessage, 0, len(c.Summaries))
	for _, sum := range c.Summaries {
		msg, _ := json.Marshal(chatMessage{Role: "system", Content: "[conversation summary] " + sum.Text})
		prefix = append(prefix, msg)
		remain -= estimateTokens(sum.Text)
	}

	// 過去 message を新しい順に budget まで採用し、古い側は summary 候補へ
	var picked []Message
	cut := -1
	for i := len(c.Messages) - 1; i >= 0; i-- {
		m := c.Messages[i]
		if m.Status != "complete" {
			continue // streaming 中断等は文脈に入れない(§10.6)
		}
		if remain-m.TokenEst < 0 {
			cut = i
			break
		}
		remain -= m.TokenEst
		picked = append(picked, m)
	}
	// picked は逆順なので戻す
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	for _, m := range picked {
		msg, _ := json.Marshal(chatMessage{Role: m.Role, Content: m.Content})
		prefix = append(prefix, msg)
		out.TokenEst += m.TokenEst
	}
	if cut >= 0 && len(c.Messages) > 0 {
		out.NeedsSummary = true
		out.SummaryFromSeq = c.Messages[0].Seq
		out.SummaryToSeq = c.Messages[cut].Seq
	}
	out.Messages = append(prefix, currentMsgs...)
	out.TokenEst += currentTokens
	return out
}
