package ibac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Judge evaluates whether a proposed agent action aligns with a recorded
// user intent. Implementations are responsible for whatever policy logic
// they want — calling an LLM, consulting a rules engine, etc.
//
// Verdict is "allow" or "deny"; any other value is treated as "deny" by
// the plugin (fail-closed). Reason is a short human-readable explanation
// surfaced in Invocation.Details so operators can see why a request was
// blocked without re-running the judge.
//
// Errors are categorized so the plugin can surface the right code:
//
//   - errors.Is(err, ErrJudgeUncertain) — judge ran but emitted an
//     unparseable / ambiguous response. Treated as a fail-closed deny
//     (HTTP 403 ibac.judge_uncertain) — different from "judge down"
//     so operators don't conflate model-output bugs with infra outages.
//
//   - any other error — transport failure, timeout, 5xx from the
//     judge endpoint. Treated as judge unavailable (HTTP 503
//     ibac.judge_unavailable).
//
// Implementations only return verdict="deny" for actual policy denials.
type Judge interface {
	Evaluate(ctx context.Context, intent, action string) (verdict, reason string, err error)
}

// ErrJudgeUncertain is the sentinel returned (wrapped) when the judge
// produced output we couldn't extract a verdict from. The plugin uses
// errors.Is to distinguish this from transport/timeout failures.
var ErrJudgeUncertain = errors.New("judge produced unparseable output")

// httpJudge calls an OpenAI chat-completions-compatible endpoint with a
// system+user prompt asking the model to compare intent vs action and
// return a structured verdict.
//
// The judge's outbound HTTP call is made directly via this client — NOT
// routed back through the authbridge listener. That structurally
// prevents the reentrancy that would otherwise occur (IBAC's judge call
// triggering IBAC's OnRequest in a loop). Defense-in-depth: every
// outbound judge request also carries `X-IBAC-Judge: 1`, which the
// plugin's OnRequest checks at the very top and short-circuits on. So
// even if a misconfiguration ever sent the judge call back through the
// proxy, IBAC would skip itself.
type httpJudge struct {
	endpoint     string
	model        string
	bearer       string
	systemPrompt string
	client       *http.Client
}

// newHTTPJudge constructs a judge that POSTs chat-completion requests to
// endpoint+"/v1/chat/completions". timeout bounds each Evaluate call
// (separately from any context deadline the caller already imposes).
// systemPrompt is the operator-overridable judge instruction; an empty
// value falls back to defaultSystemPrompt.
func newHTTPJudge(endpoint, model, bearer, systemPrompt string, timeout time.Duration) *httpJudge {
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}
	return &httpJudge{
		endpoint:     strings.TrimRight(endpoint, "/"),
		model:        model,
		bearer:       bearer,
		systemPrompt: systemPrompt,
		client:       &http.Client{Timeout: timeout},
	}
}

// Wire shapes for the OpenAI chat-completions API. We model only the
// fields we consume.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// verdictPayload is the JSON shape we ask the judge model to emit inside
// the chat-completion's content. Tight schema (two fields, both strings)
// keeps parsing reliable across model families.
type verdictPayload struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// defaultSystemPrompt instructs the judge to compare intent vs action
// and emit a JSON verdict. Conservative-by-default: when ambiguous, the
// model should deny.
const defaultSystemPrompt = `You are a security policy judge for an AI agent platform.
The user expressed an intent. The agent is about to perform an outbound action.
Decide whether the action aligns with the user's intent.

Respond with JSON only, in exactly this shape:
  {"verdict": "allow", "reason": "<one sentence>"}
  or
  {"verdict": "deny", "reason": "<one sentence>"}

Do not include any text outside the JSON object.
Be conservative — if the action is unrelated to the intent, includes
unfamiliar destinations, or looks like data exfiltration, deny.`

// Evaluate sends a chat-completion request to the configured judge LLM
// and parses the verdict. ctx caller-provided deadlines apply on top of
// the client's per-call timeout.
func (j *httpJudge) Evaluate(ctx context.Context, intent, action string) (string, string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       j.model,
		Temperature: 0,
		MaxTokens:   200,
		Messages: []chatMessage{
			{Role: "system", Content: j.systemPrompt},
			{Role: "user", Content: fmt.Sprintf("USER_INTENT:\n%s\n\nPROPOSED_ACTION:\n%s", intent, action)},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal judge request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("build judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if j.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+j.bearer)
	}
	// Defense-in-depth: if the judge call ever loops back through the
	// proxy (operator misconfiguration), IBAC's OnRequest sees this
	// header and short-circuits without judging again.
	req.Header.Set("X-IBAC-Judge", "1")

	resp, err := j.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("judge call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Limit body read so a misbehaving upstream can't OOM the sidecar.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", "", fmt.Errorf("judge returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var cr chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&cr); err != nil {
		return "", "", fmt.Errorf("decode judge response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", "", fmt.Errorf("%w: judge response had no choices", ErrJudgeUncertain)
	}

	verdict, reason, err := parseVerdict(cr.Choices[0].Message.Content)
	if err != nil {
		return "", "", err
	}
	return verdict, reason, nil
}

// parseVerdict pulls the {verdict, reason} object out of the model's
// raw content. Models often wrap JSON in markdown code fences or prefix
// it with prose; we extract the first {...} block before unmarshaling.
//
// All failure modes wrap ErrJudgeUncertain so the caller can route them
// to a fail-closed-deny (403) instead of judge-unavailable (503): the
// judge is up and responded, it just produced something we can't act on.
func parseVerdict(content string) (string, string, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return "", "", fmt.Errorf("%w: no JSON object in content: %q",
			ErrJudgeUncertain, truncate(content, 200))
	}
	var p verdictPayload
	if err := json.Unmarshal([]byte(content[start:end+1]), &p); err != nil {
		return "", "", fmt.Errorf("%w: unmarshal: %v (content %q)",
			ErrJudgeUncertain, err, truncate(content, 200))
	}
	v := strings.ToLower(strings.TrimSpace(p.Verdict))
	if v != "allow" && v != "deny" {
		return "", "", fmt.Errorf("%w: unrecognized verdict %q (content %q)",
			ErrJudgeUncertain, p.Verdict, truncate(content, 200))
	}
	return v, strings.TrimSpace(p.Reason), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
