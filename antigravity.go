package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type tokenSource interface {
	accessToken(ctx context.Context, rejected string) (string, error)
}

type antigravity struct {
	client  *http.Client
	auth    tokenSource
	baseURL string

	mu      sync.Mutex
	project string
}

type upstreamError struct {
	status int
	action string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("Antigravity %s returned HTTP %d", e.action, e.status)
}

func (a *antigravity) post(ctx context.Context, action string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var rejected string
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.auth.accessToken(ctx, rejected)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1internal:"+action, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "agy-api/1.0")
		res, err := a.client.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusOK {
			return res, nil
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return nil, &upstreamError{res.StatusCode, action}
		}
		rejected = token
	}
	return nil, errors.New("Antigravity rejected the OAuth token")
}

func (a *antigravity) projectID(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project != "" {
		return a.project, nil
	}
	res, err := a.post(ctx, "loadCodeAssist", map[string]any{
		"metadata": map[string]string{"ideType": "ANTIGRAVITY"},
	})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var result struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Antigravity project: %w", err)
	}
	if len(result.Project) > 0 && json.Unmarshal(result.Project, &a.project) != nil {
		var project struct{ ID string }
		if err := json.Unmarshal(result.Project, &project); err != nil {
			return "", fmt.Errorf("decode Antigravity project ID: %w", err)
		}
		a.project = project.ID
	}
	// Some accounts discover models without an explicit project.
	return a.project, nil
}

func (a *antigravity) models(ctx context.Context) ([]string, error) {
	project, err := a.projectID(ctx)
	if err != nil {
		return nil, err
	}
	res, err := a.post(ctx, "fetchAvailableModels", struct {
		Project string `json:"project,omitempty"`
	}{project})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var result struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode Antigravity models: %w", err)
	}
	if result.Models == nil {
		return nil, errors.New("Antigravity returned no models field")
	}
	models := make([]string, 0, len(result.Models))
	for id := range result.Models {
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

type geminiPart struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type generationRequest struct {
	Session  string          `json:"sessionId,omitempty"`
	Contents []geminiContent `json:"contents"`
	System   *geminiContent  `json:"systemInstruction,omitempty"`
	Config   struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"topP,omitempty"`
		MaxTokens   int      `json:"maxOutputTokens,omitempty"`
		Stop        []string `json:"stopSequences,omitempty"`
	} `json:"generationConfig"`
}

type tokenUsage struct {
	Prompt     int `json:"promptTokenCount"`
	Completion int `json:"candidatesTokenCount"`
	Total      int `json:"totalTokenCount"`
}

type geminiEvent struct {
	Error      json.RawMessage `json:"error"`
	Candidates []struct {
		Content geminiContent `json:"content"`
		Finish  string        `json:"finishReason"`
	} `json:"candidates"`
	Usage          *tokenUsage `json:"usageMetadata"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

func (a *antigravity) generate(ctx context.Context, model string, input generationRequest) (*http.Response, error) {
	project, err := a.projectID(ctx)
	if err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	requestID := fmt.Sprintf("agent-%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	for _, message := range input.Contents {
		if message.Role == "user" && len(message.Parts) > 0 {
			sum := sha256.Sum256([]byte(message.Parts[0].Text))
			input.Session = fmt.Sprintf("-%d", binary.BigEndian.Uint64(sum[:8])>>1)
			break
		}
	}
	return a.post(ctx, "streamGenerateContent?alt=sse", struct {
		Model     string            `json:"model"`
		Project   string            `json:"project,omitempty"`
		Type      string            `json:"requestType"`
		UserAgent string            `json:"userAgent"`
		ID        string            `json:"requestId"`
		Request   generationRequest `json:"request"`
	}{model, project, "agent", "antigravity", requestID, input})
}

func readGeneration(body io.Reader, emit func(geminiEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data []string
	finished := false
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := strings.Join(data, "\n")
		data = data[:0]
		if raw == "[DONE]" {
			return nil
		}
		var envelope struct {
			geminiEvent
			Response *geminiEvent `json:"response"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return errors.New("invalid Antigravity generation event")
		}
		event := envelope.geminiEvent
		if envelope.Response != nil {
			event = *envelope.Response
		}
		for _, failure := range []json.RawMessage{envelope.Error, event.Error} {
			if len(failure) > 0 && string(failure) != "null" {
				return errors.New("Antigravity returned a generation error")
			}
		}
		reason := finishReason(event)
		finished = finished || reason != ""
		if len(event.Candidates) == 0 && event.Usage == nil && reason == "" {
			return nil
		}
		return emit(event)
	}
	eventSize := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			eventSize = 0
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			eventSize += len(value) + 1
			if eventSize > 4<<20 {
				return errors.New("Antigravity generation event too large")
			}
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil {
		return err
	}
	if !finished {
		return errors.New("Antigravity ended generation before sending a finish reason")
	}
	return nil
}
