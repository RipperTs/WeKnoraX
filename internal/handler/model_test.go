package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type modelHandlerServiceStub struct {
	interfaces.ModelService
	models  []*types.Model
	created *types.Model
}

func (s *modelHandlerServiceStub) ListModels(context.Context) ([]*types.Model, error) {
	return s.models, nil
}

func (s *modelHandlerServiceStub) CreateModel(_ context.Context, model *types.Model) error {
	s.created = model
	return nil
}

func newModelHandlerContext(method, target string, body io.Reader) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, target, body)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(types.TenantIDContextKey.String(), uint64(1))
	return c, recorder
}

func TestListModelsOrdersBySortOrderThenCreatedAt(t *testing.T) {
	older := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	service := &modelHandlerServiceStub{models: []*types.Model{
		{ID: "higher-sort", SortOrder: 20, CreatedAt: newer},
		{ID: "older-same-sort", SortOrder: 10, CreatedAt: older},
		{ID: "newer-same-sort", SortOrder: 10, CreatedAt: newer},
	}}
	c, recorder := newModelHandlerContext(http.MethodGet, "/models", nil)

	NewModelHandler(service).ListModels(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Data, 3)
	assert.Equal(t, "newer-same-sort", response.Data[0].ID)
	assert.Equal(t, "older-same-sort", response.Data[1].ID)
	assert.Equal(t, "higher-sort", response.Data[2].ID)
}

func TestCreateModelDefaultsSortOrder(t *testing.T) {
	service := &modelHandlerServiceStub{}
	body := bytes.NewBufferString(`{
		"name":"test-model",
		"type":"KnowledgeQA",
		"source":"remote",
		"parameters":{}
	}`)
	c, recorder := newModelHandlerContext(http.MethodPost, "/models", body)

	NewModelHandler(service).CreateModel(c)

	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	require.NotNil(t, service.created)
	assert.Equal(t, types.DefaultModelSortOrder, service.created.SortOrder)
}

func TestModelUpdateRequestDisplayNamePresence(t *testing.T) {
	var omitted UpdateModelRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"gpt-4o"}`), &omitted))
	assert.Nil(t, omitted.DisplayName)

	var cleared UpdateModelRequest
	require.NoError(t, json.Unmarshal([]byte(`{"display_name":""}`), &cleared))
	require.NotNil(t, cleared.DisplayName)
	assert.Equal(t, "", *cleared.DisplayName)
}

func TestParseModelDebugOptionsPreservesExplicitThinkingFalse(t *testing.T) {
	opts, err := parseModelDebugOptions(`{"thinking":false,"temperature":0,"max_tokens":256}`)
	require.NoError(t, err)
	require.NotNil(t, opts.Thinking)
	assert.False(t, *opts.Thinking)
	require.NotNil(t, opts.Temperature)
	assert.Zero(t, *opts.Temperature)
	require.NotNil(t, opts.MaxTokens)
	assert.Equal(t, 256, *opts.MaxTokens)
}

func TestParseModelDebugOptionsRejectsOutOfRangeValues(t *testing.T) {
	_, err := parseModelDebugOptions(`{"top_p":0}`)
	require.ErrorContains(t, err, "top_p")
}

func TestRedactedDebugConfig(t *testing.T) {
	got := redactedDebugConfig(map[string]string{
		"thinking_control": "enable_thinking",
		"secret_key":       "do-not-leak",
		"access_token":     "do-not-leak-either",
	})
	assert.Equal(t, "enable_thinking", got["thinking_control"])
	assert.Equal(t, "[REDACTED]", got["secret_key"])
	assert.Equal(t, "[REDACTED]", got["access_token"])
}

func TestConsumeModelDebugChatStream(t *testing.T) {
	stream := make(chan types.StreamResponse, 5)
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeThinking, Content: "reason "}
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeThinking, Content: "more", Done: true}
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Content: "answer "}
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Content: "done"}
	stream <- types.StreamResponse{
		ResponseType: types.ResponseTypeAnswer,
		Done:         true,
		FinishReason: "stop",
		Usage:        &types.TokenUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}
	close(stream)

	got, err := consumeModelDebugChatStream(stream)
	require.NoError(t, err)
	assert.Equal(t, "reason more", got.ReasoningContent)
	assert.Equal(t, "answer done", got.Content)
	assert.Equal(t, "stop", got.FinishReason)
	require.NotNil(t, got.Usage)
	assert.Equal(t, 7, got.Usage.TotalTokens)
	assert.Len(t, got.StreamEvents, 5)
}
