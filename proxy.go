package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type proxy struct {
	key      string
	upstream *antigravity
}

func (p proxy) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", p.models)
	mux.HandleFunc("POST /v1/chat/completions", p.chat)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, key, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(key), []byte(p.key)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "A valid API key is required.")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (p proxy) models(w http.ResponseWriter, r *http.Request) {
	ids, err := p.upstream.models(r.Context())
	if err != nil {
		upstreamFailure(w, err)
		return
	}
	list := make([]object, 0, len(ids))
	for _, id := range ids {
		list = append(list, object{"id": id, "object": "model", "created": 0, "owned_by": "antigravity"})
	}
	writeJSON(w, http.StatusOK, object{"object": "list", "data": list})
}

type object map[string]any

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	N                   int             `json:"n"`
}

func (c chatRequest) generation() (generationRequest, error) {
	var req generationRequest
	if strings.TrimSpace(c.Model) == "" || len(c.Messages) == 0 {
		return req, errors.New("model and messages are required")
	}
	if c.N != 0 && c.N != 1 {
		return req, errors.New("only n=1 is supported")
	}
	if c.MaxTokens < 0 || c.MaxCompletionTokens < 0 {
		return req, errors.New("token limits must be positive")
	}
	for _, msg := range c.Messages {
		parts, err := textParts(msg.Content)
		if err != nil {
			return req, err
		}
		var role string
		switch msg.Role {
		case "system", "developer":
			if req.System == nil {
				req.System = &geminiContent{}
			}
			req.System.Parts = append(req.System.Parts, parts...)
			continue
		case "user":
			role = "user"
		case "assistant":
			role = "model"
		default:
			return req, fmt.Errorf("unsupported role %q", msg.Role)
		}
		req.Contents = append(req.Contents, geminiContent{Role: role, Parts: parts})
	}
	if len(req.Contents) == 0 {
		return req, errors.New("messages must include a user or assistant message")
	}
	req.Config.Temperature = c.Temperature
	req.Config.TopP = c.TopP
	req.Config.MaxTokens = c.MaxCompletionTokens
	if req.Config.MaxTokens == 0 {
		req.Config.MaxTokens = c.MaxTokens
	}
	if len(c.Stop) > 0 && string(c.Stop) != "null" {
		var value string
		if json.Unmarshal(c.Stop, &value) == nil {
			req.Config.Stop = []string{value}
		} else if json.Unmarshal(c.Stop, &req.Config.Stop) != nil {
			return req, errors.New("stop must be a string or an array of strings")
		}
	}
	return req, nil
}

func textParts(raw json.RawMessage) ([]geminiPart, error) {
	var value *string
	if json.Unmarshal(raw, &value) == nil && value != nil {
		return []geminiPart{{Text: *value}}, nil
	}
	var data []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(raw, &data); err == nil && len(data) > 0 {
		parts := make([]geminiPart, 0, len(data))
		for _, part := range data {
			if part.Type != "text" || part.Text == nil {
				return nil, errors.New("only text message parts are supported")
			}
			parts = append(parts, geminiPart{Text: *part.Text})
		}
		return parts, nil
	}
	return nil, errors.New("message content must be a string or an array of text parts")
}

func (p proxy) chat(w http.ResponseWriter, r *http.Request) {
	var input chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Expected one JSON object.")
		return
	}
	req, err := input.generation()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	id, err := randomString()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "Could not allocate a completion ID.")
		return
	}
	res, err := p.upstream.generate(r.Context(), input.Model, req)
	if err != nil {
		upstreamFailure(w, err)
		return
	}
	defer res.Body.Close()
	completion := object{
		"id": "chatcmpl-" + id, "created": time.Now().Unix(), "model": input.Model,
	}
	if input.Stream {
		streamChat(w, res.Body, completion, input.StreamOptions.IncludeUsage)
		return
	}
	completion["object"] = "chat.completion"
	var output strings.Builder
	var finish string
	var usage *tokenUsage
	if err := readGeneration(res.Body, func(event geminiEvent) error {
		text, reason := eventResult(event)
		if len(text) > (16<<20)-output.Len() {
			return errors.New("Antigravity completion too large")
		}
		output.WriteString(text)
		if reason != "" {
			finish = reason
		}
		if event.Usage != nil {
			usage = event.Usage
		}
		return nil
	}); err != nil {
		upstreamFailure(w, err)
		return
	}
	completion["choices"] = []object{{
		"index": 0, "message": object{"role": "assistant", "content": output.String()}, "finish_reason": finish,
	}}
	if usage != nil {
		completion["usage"] = usageResult(usage)
	}
	writeJSON(w, http.StatusOK, completion)
}

func streamChat(w http.ResponseWriter, body io.Reader, completion object, includeUsage bool) {
	flush, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "Streaming is unavailable.")
		return
	}
	completion["object"] = "chat.completion.chunk"
	var usage *tokenUsage
	if includeUsage {
		completion["usage"] = nil
	}
	started := false
	emit := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			started = true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flush.Flush()
		return nil
	}
	chunk := func(delta object, finish any) error {
		completion["choices"] = []object{{"index": 0, "delta": delta, "finish_reason": finish}}
		return emit(completion)
	}
	err := readGeneration(body, func(event geminiEvent) error {
		if !started {
			if err := chunk(object{"role": "assistant", "content": ""}, nil); err != nil {
				return err
			}
		}
		if event.Usage != nil {
			usage = event.Usage
		}
		text, finish := eventResult(event)
		if text != "" {
			if err := chunk(object{"content": text}, nil); err != nil {
				return err
			}
		}
		if finish != "" {
			return chunk(object{}, finish)
		}
		return nil
	})
	if err != nil {
		if started {
			emit(errorBody("upstream_error", "Antigravity interrupted generation."))
		} else {
			upstreamFailure(w, err)
		}
		return
	}
	if includeUsage && usage != nil {
		completion["choices"] = []object{}
		completion["usage"] = usageResult(usage)
		if err := emit(completion); err != nil {
			return
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush.Flush()
}

func eventResult(event geminiEvent) (string, string) {
	var text strings.Builder
	if len(event.Candidates) > 0 {
		for _, part := range event.Candidates[0].Content.Parts {
			if !part.Thought {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String(), finishReason(event)
}

func finishReason(event geminiEvent) string {
	if len(event.Candidates) == 0 {
		if event.PromptFeedback.BlockReason != "" && event.PromptFeedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
			return "content_filter"
		}
		return ""
	}
	switch event.Candidates[0].Finish {
	case "", "FINISH_REASON_UNSPECIFIED":
		return ""
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MODEL_ARMOR":
		return "content_filter"
	default:
		return "stop"
	}
}

func usageResult(usage *tokenUsage) object {
	return object{"prompt_tokens": usage.Prompt, "completion_tokens": usage.Completion, "total_tokens": usage.Total}
}

func upstreamFailure(w http.ResponseWriter, err error) {
	log.Printf("Upstream request: %s", upstreamLogError(err))
	var remote *upstreamError
	if errors.As(err, &remote) {
		switch remote.status {
		case 400, 403, 404, 429:
			writeError(w, remote.status, "upstream_error", remote.Error())
			return
		}
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "Antigravity request failed. See the server log.")
}

func errorBody(kind, message string) object {
	return object{"error": object{"message": message, "type": kind, "code": kind}}
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, errorBody(kind, message))
}

func writeJSON(w http.ResponseWriter, status int, body object) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
