package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// openAIService OpenAI 兼容的 Embedding 服务实现
// 支持 OpenAI API 和 LM Studio 等兼容实现
type openAIService struct {
	baseURL   string
	apiKey    string
	model     string
	dimension int
	client    *http.Client
}

func newOpenAIService(cfg Config) (*openAIService, error) {
	timeout := 30 * time.Second
	return &openAIService{
		baseURL:   cfg.BaseURL,
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		dimension: cfg.Dimension,
		client: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// embeddingRequest OpenAI Embedding API 请求
type embeddingRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

// embeddingResponse OpenAI Embedding API 响应
type embeddingResponse struct {
	Data  []embeddingData `json:"data"`
	Error *apiError       `json:"error,omitempty"`
}

type embeddingData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (s *openAIService) GenerateEmbedding(ctx context.Context, text string) ([]float32, error) {
	results, err := s.GenerateEmbeddings(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return results[0], nil
}

func (s *openAIService) GenerateEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// 构建请求
	reqBody := embeddingRequest{
		Input: texts,
		Model: s.model,
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// 构建 HTTP 请求
	url := s.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}

	// 发送请求
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var embResp embeddingResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if embResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", embResp.Error.Message)
	}

	// 按 index 排序结果
	results := make([][]float32, len(texts))
	for _, data := range embResp.Data {
		if data.Index >= 0 && data.Index < len(results) {
			results[data.Index] = data.Embedding
		}
	}

	// 检查是否所有结果都已填充
	for i, r := range results {
		if r == nil {
			return nil, fmt.Errorf("missing embedding for input %d", i)
		}
	}

	return results, nil
}

func (s *openAIService) Dimension() int {
	return s.dimension
}
