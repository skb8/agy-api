package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxy(t *testing.T) {
	var refreshes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "saved-refresh" || r.Form.Get("grant_type") != "refresh_token" {
				t.Error("unexpected OAuth refresh request")
			}
			fmt.Fprint(w, `{"access_token":"fresh-token","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer rejected-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Error("unexpected upstream authorization")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"cloudaicompanionProject":{"id":"project"}}`)
		case "/v1internal:fetchAvailableModels":
			var body struct{ Project string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Project != "project" {
				t.Error("model list request missing project")
			}
			fmt.Fprint(w, `{"models":{"z-model":{},"a-model":{}}}`)
		case "/v1internal:streamGenerateContent":
			var body struct {
				Model, Project string
				Request        generationRequest
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "a-model" || body.Project != "project" ||
				len(body.Request.Contents) != 1 || len(body.Request.Contents[0].Parts) != 1 || body.Request.Contents[0].Parts[0].Text != "hello" || body.Request.System == nil {
				t.Error("unexpected generation request")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {}\n\n")
			fmt.Fprint(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"secret thought","thought":true},{"text":"hello"}]}}]}}`+"\n\n")
			fmt.Fprint(w, `data: {"candidates":[{"content":{"parts":[{"text":" world"}]},"finishReason":"STOP"}]}`+"\n\n")
			fmt.Fprint(w, `data: {"response":{"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}}`+"\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), ".env")
	env, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	oauth := &googleOAuth{
		env: env, client: upstream.Client(), tokenURL: upstream.URL + "/token", clientID: "client",
		tokens: oauthTokens{"rejected-token", "saved-refresh", time.Now().Add(time.Hour)},
	}
	api := proxy{"local-key", &antigravity{client: upstream.Client(), auth: oauth, baseURL: upstream.URL}}.routes()
	call := func(method, path, key, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		api.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/v1/models", "wrong-key", ""); w.Code != 401 {
		t.Fatalf("unauthenticated status = %d", w.Code)
	}
	if w := call("GET", "/v1/models", "local-key", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"a-model"`) {
		t.Fatalf("model response: %d %s", w.Code, w.Body)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
	saved, err := readEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.values["ANTIGRAVITY_ACCESS_TOKEN"] != "fresh-token" || saved.values["ANTIGRAVITY_REFRESH_TOKEN"] != "saved-refresh" {
		t.Fatal("the OAuth values were not persisted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf(".env mode = %v", info.Mode())
	}

	body := `{"model":"a-model","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hello"}]`
	w := call("POST", "/v1/chat/completions", "local-key", body+"}")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello world") || strings.Contains(w.Body.String(), "secret thought") {
		t.Fatalf("chat response: %d %s", w.Code, w.Body)
	}
	w = call("POST", "/v1/chat/completions", "local-key", body+`,"stream":true,"stream_options":{"include_usage":true}}`)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" ||
		!strings.Contains(w.Body.String(), `"finish_reason":"stop"`) || !strings.Contains(w.Body.String(), `"total_tokens":9`) ||
		!strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") || strings.Contains(w.Body.String(), "secret thought") {
		t.Fatalf("stream response: %d %s", w.Code, w.Body)
	}
}

func TestInterruptedStream(t *testing.T) {
	w := httptest.NewRecorder()
	streamChat(w, strings.NewReader(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}`+"\n\n"), object{}, false)
	if !strings.Contains(w.Body.String(), "upstream_error") || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("interrupted stream looked successful: %s", w.Body)
	}
}
