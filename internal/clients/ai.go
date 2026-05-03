package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

type AIClient struct {
	baseURL string
	http    *http.Client
}

func NewAIClient(baseURL string) *AIClient {
	return &AIClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// ChatRequest is sent to POST {ai}/chat
type ChatRequest struct {
	TenantID         string        `json:"tenant_id"`
	ConversationID   string        `json:"conversation_id"`
	AgentID          string        `json:"agent_id"`
	SystemPrompt     string        `json:"system_prompt"`
	Model            string        `json:"model"`
	Temperature      float64       `json:"temperature"`
	History          []ChatMessage `json:"history"`
	Message          string        `json:"message"`
	KnowledgeBaseIDs []string      `json:"knowledge_base_ids"`
	// MentionSources controls whether the AI is allowed to cite source
	// filenames in its reply. True for the dashboard playground (so staff
	// can verify grounding), false for every customer-facing channel
	// (LINE, Facebook, etc.) so internal filenames don't leak.
	MentionSources bool `json:"mention_sources"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Reply      string   `json:"reply"`
	Sources    []string `json:"sources,omitempty"`
	TokensUsed int      `json:"tokens_used,omitempty"`
}

func (c *AIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ai service: %s: %s", resp.Status, string(b))
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

type IngestResponse struct {
	Chunks int    `json:"chunks"`
	Stored int    `json:"stored"`
	Source string `json:"source,omitempty"`
}

// IngestFile uploads a file to the AI service for ingestion (chunk + embed + upsert).
func (c *AIClient) IngestFile(
	ctx context.Context,
	tenantID, knowledgeBaseID, filename string,
	content []byte,
) (*IngestResponse, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("tenant_id", tenantID)
	_ = w.WriteField("knowledge_base_id", knowledgeBaseID)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(content); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/ingest/file", &buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ai ingest: %s: %s", resp.Status, string(b))
	}
	var out IngestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteKnowledgeBase removes all vectors for a tenant + KB pair from Qdrant.
func (c *AIClient) DeleteKnowledgeBase(ctx context.Context, tenantID, knowledgeBaseID string) error {
	body, _ := json.Marshal(map[string]string{
		"tenant_id":         tenantID,
		"knowledge_base_id": knowledgeBaseID,
	})
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/ingest/kb/delete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ai delete kb: %s: %s", resp.Status, string(b))
	}
	return nil
}

// Health pings the AI service.
func (c *AIClient) Health(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("ai health: %s", resp.Status)
	}
	return nil
}
