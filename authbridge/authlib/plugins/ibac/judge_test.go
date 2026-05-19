package ibac

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Test that a successful judge call decodes the verdict from a real
// OpenAI-shaped chat-completion response.
func TestHTTPJudge_AllowVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity-check the request shape; failing this would mean the
		// judge isn't sending what an OpenAI-compatible endpoint expects.
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get("X-IBAC-Judge"); got != "1" {
			t.Errorf("X-IBAC-Judge = %q, want 1 (defense-in-depth sentinel must be set)", got)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q, want test-model", req.Model)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		// The user message must contain BOTH the intent and the action,
		// because the judge can't decide alignment with only one.
		if !strings.Contains(req.Messages[1].Content, "summarize emails") ||
			!strings.Contains(req.Messages[1].Content, "POST evil-server") {
			t.Errorf("user message missing intent or action: %q", req.Messages[1].Content)
		}

		json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: `{"verdict": "allow", "reason": "GET to known service"}`}},
			},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "test-model", "", "", time.Second)
	verdict, reason, err := j.Evaluate(context.Background(), "summarize emails", "POST evil-server")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if verdict != "allow" {
		t.Errorf("verdict = %q, want allow", verdict)
	}
	if reason != "GET to known service" {
		t.Errorf("reason = %q, want 'GET to known service'", reason)
	}
}

// Models often wrap their JSON in markdown code fences or prefix it
// with prose. The judge must extract the JSON object regardless.
func TestHTTPJudge_VerdictWrappedInProse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{
					Role: "assistant",
					Content: "Sure, here is my judgment:\n```json\n" +
						`{"verdict": "deny", "reason": "POST to unknown server"}` +
						"\n```",
				}},
			},
		})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second)
	verdict, reason, err := j.Evaluate(context.Background(), "i", "a")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if verdict != "deny" || reason != "POST to unknown server" {
		t.Errorf("verdict=%q reason=%q, want deny / POST to unknown server", verdict, reason)
	}
}

// 5xx from the judge endpoint must surface as an error so the plugin
// fails closed instead of allowing the request through.
func TestHTTPJudge_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "judge overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatalf("Evaluate returned nil error on HTTP 503; expected fail-closed signal")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %q, want it to mention HTTP 503 status", err)
	}
}

// Connection-refused (no server listening) must also fail closed.
func TestHTTPJudge_NetworkError(t *testing.T) {
	// Bind a server, capture its URL, then close it so the URL is
	// guaranteed unreachable. Avoids picking a random port that might
	// be in use elsewhere on the runner.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	j := newHTTPJudge(url, "m", "", "", 200*time.Millisecond)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatalf("Evaluate returned nil error on connection refused; expected fail-closed signal")
	}
}

// Bearer token must be forwarded to the judge endpoint.
func TestHTTPJudge_BearerForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(chatResponse{Choices: []struct {
			Message chatMessage `json:"message"`
		}{
			{Message: chatMessage{Content: `{"verdict":"allow","reason":"ok"}`}},
		}})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "secret-token", "", time.Second)
	if _, _, err := j.Evaluate(context.Background(), "i", "a"); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want 'Bearer secret-token'", got)
	}
}

// Garbage in the model's content (no JSON object at all) must fail
// closed — the plugin treats parse errors as judge unavailable.
func TestHTTPJudge_NoJSONInContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{Choices: []struct {
			Message chatMessage `json:"message"`
		}{
			{Message: chatMessage{Content: "I don't know."}},
		}})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatalf("Evaluate returned nil error on no-JSON content; expected fail-closed signal")
	}
}

// Verdict that's neither "allow" nor "deny" must fail closed —
// e.g., a model that emits {"verdict": "maybe"} should not be
// silently treated as allow.
func TestHTTPJudge_UnrecognizedVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{Choices: []struct {
			Message chatMessage `json:"message"`
		}{
			{Message: chatMessage{Content: `{"verdict": "maybe", "reason": "huh"}`}},
		}})
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", time.Second)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	if err == nil {
		t.Fatalf("Evaluate returned nil error on unrecognized verdict; expected fail-closed signal")
	}
}

// parseVerdict unit tests (no HTTP server needed) — exercise the
// extraction paths directly.
func TestParseVerdict_ExtractsFromVariousShapes(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantVerdict string
		wantErr     bool
	}{
		{"plain JSON", `{"verdict": "allow", "reason": "ok"}`, "allow", false},
		{"with prose prefix", `Sure: {"verdict": "deny", "reason": "no"}`, "deny", false},
		{"with code fence", "```json\n{\"verdict\":\"allow\",\"reason\":\"x\"}\n```", "allow", false},
		{"upper-case verdict", `{"verdict": "ALLOW", "reason": "x"}`, "allow", false},
		{"empty content", ``, "", true},
		{"no JSON object", `I'm not sure.`, "", true},
		{"malformed JSON", `{"verdict": "allow"`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _, err := parseVerdict(tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && v != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", v, tc.wantVerdict)
			}
		})
	}
}

// Verify the per-call timeout is actually enforced. A judge endpoint
// that never responds must trip the http.Client.Timeout — without this
// the plugin would wait indefinitely on a wedged judge LLM and tie up
// the plugin pipeline goroutine. The error is NOT ErrJudgeUncertain
// (the judge didn't respond at all, so it's an availability issue).
func TestHTTPJudge_TimeoutEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the judge timeout. The CloseNotify channel
		// lets the goroutine exit when the test server tears down.
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	start := time.Now()
	j := newHTTPJudge(srv.URL, "m", "", "", 200*time.Millisecond)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Evaluate returned nil error after slow-server timeout; expected fail-closed signal")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Evaluate took %v, expected the 200ms timeout to trip well before 1.5s", elapsed)
	}
	// The judge didn't respond at all — this is an availability issue,
	// not an "uncertain output" case. ErrJudgeUncertain must NOT be
	// part of this error chain.
	if errors.Is(err, ErrJudgeUncertain) {
		t.Errorf("timeout error wrongly wrapped ErrJudgeUncertain; "+
			"plugin would route this to 403 instead of 503. err=%v", err)
	}
}

// Confirm that the body excerpt limit works — io.LimitReader on the
// response body shouldn't be triggered by a normal-sized response,
// but we want to make sure a 100KB judge response doesn't blow up.
func TestHTTPJudge_LargeResponseTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Emit a chat response with a huge content field. The decoder's
		// LimitReader caps at 64KB; an OpenAI completion this size is
		// pathological, and we just want to verify it doesn't OOM.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"`+
			strings.Repeat("x", 200000)+`"}}]}`)
	}))
	defer srv.Close()

	j := newHTTPJudge(srv.URL, "m", "", "", 2*time.Second)
	_, _, err := j.Evaluate(context.Background(), "i", "a")
	// We expect a parse error (no JSON verdict in the content), not
	// a panic or hang. Either an error from io.UnexpectedEOF (limit
	// reader truncates mid-string) or a "no JSON object" error is
	// acceptable; the contract is "doesn't crash".
	if err == nil {
		t.Errorf("expected error from oversized response, got nil")
	}
}
