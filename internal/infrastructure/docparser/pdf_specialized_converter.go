package docparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	pdfSpecializedParserEnabledEnv  = "PDF_SPECIALIZED_PARSER_ENABLED"
	pdfSpecializedParserEndpointEnv = "PDF_SPECIALIZED_PARSER_ENDPOINT"
	pdfSpecializedPollInterval      = time.Second
)

// PDFSpecializedReader calls the asynchronous PDF parsing service configured
// by the deployment environment.
type PDFSpecializedReader struct {
	endpoint string
	client   *http.Client
}

// NewPDFSpecializedReader creates a reader for the configured service address.
func NewPDFSpecializedReader(endpoint string) *PDFSpecializedReader {
	return &PDFSpecializedReader{
		endpoint: strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		client: &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func pdfSpecializedParserConfig() (bool, string) {
	enabled := parseBoolOr(os.Getenv(pdfSpecializedParserEnabledEnv), false)
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(pdfSpecializedParserEndpointEnv)), "/")
	return enabled, endpoint
}

func (c *PDFSpecializedReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if c.endpoint == "" {
		return nil, fmt.Errorf("PDF 专用解析服务地址未配置")
	}
	if len(req.FileContent) == 0 {
		return nil, fmt.Errorf("PDF 文件内容为空")
	}

	logger.Infof(ctx, "[PDFSpecialized] Submitting file=%s size=%d via %s",
		req.FileName, len(req.FileContent), c.endpoint)

	taskID, err := c.submit(ctx, req)
	if err != nil {
		return nil, err
	}

	markdown, err := c.poll(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(markdown) == "" {
		return nil, fmt.Errorf("PDF 专用解析服务返回空 Markdown")
	}

	logger.Infof(ctx, "[PDFSpecialized] Parsed successfully, task=%s markdown=%d chars",
		taskID, len(markdown))

	return &types.ReadResult{
		MarkdownContent: markdown,
		Metadata: map[string]string{
			"parser": PDFSpecializedEngineName,
		},
	}, nil
}

type pdfSpecializedSubmitResponse struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

func (c *PDFSpecializedReader) submit(ctx context.Context, req *types.ReadRequest) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filePart, err := writer.CreateFormFile("pdf_file", pdfSpecializedFileName(req.FileName))
	if err != nil {
		return "", fmt.Errorf("创建 PDF 上传表单: %w", err)
	}
	if _, err := filePart.Write(req.FileContent); err != nil {
		return "", fmt.Errorf("写入 PDF 上传内容: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("完成 PDF 上传表单: %w", err)
	}

	requestURL, err := url.Parse(c.endpoint + "/pdf_parse")
	if err != nil {
		return "", fmt.Errorf("解析 PDF 专用解析服务地址: %w", err)
	}
	query := requestURL.Query()
	query.Set("parse_method", "auto")
	query.Set("is_json_md_dump", "false")
	requestURL.RawQuery = query.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), &body)
	if err != nil {
		return "", fmt.Errorf("创建 PDF 解析请求: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("提交 PDF 解析任务: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 PDF 解析任务响应: %w", err)
	}

	var result pdfSpecializedSubmitResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析 PDF 解析任务响应: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("提交 PDF 解析任务失败，HTTP %d: %s",
			resp.StatusCode, pdfSpecializedErrorMessage(result.Error, respBody))
	}
	if strings.TrimSpace(result.TaskID) == "" {
		return "", fmt.Errorf("PDF 专用解析服务未返回 task_id")
	}
	return result.TaskID, nil
}

type pdfSpecializedTaskResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Result struct {
		Markdown string `json:"markdown"`
	} `json:"result"`
}

func (c *PDFSpecializedReader) poll(ctx context.Context, taskID string) (string, error) {
	for {
		result, err := c.getTask(ctx, taskID)
		if err != nil {
			return "", err
		}

		switch result.Status {
		case "completed":
			return result.Result.Markdown, nil
		case "failed":
			return "", fmt.Errorf("PDF 解析任务失败: %s", pdfSpecializedErrorMessage(result.Error, nil))
		case "pending", "processing":
			sleepCtx(ctx, pdfSpecializedPollInterval)
			if err := ctx.Err(); err != nil {
				return "", fmt.Errorf("等待 PDF 解析任务完成: %w", err)
			}
		default:
			return "", fmt.Errorf("PDF 专用解析服务返回未知任务状态: %q", result.Status)
		}
	}
}

func (c *PDFSpecializedReader) getTask(ctx context.Context, taskID string) (*pdfSpecializedTaskResponse, error) {
	taskURL := c.endpoint + "/task/" + url.PathEscape(taskID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, taskURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 PDF 任务状态请求: %w", err)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("查询 PDF 解析任务: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 PDF 任务状态响应: %w", err)
	}

	var result pdfSpecializedTaskResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析 PDF 任务状态响应: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if result.Status != "failed" {
			return nil, fmt.Errorf("查询 PDF 解析任务失败，HTTP %d: %s",
				resp.StatusCode, pdfSpecializedErrorMessage(result.Error, respBody))
		}
	}
	return &result, nil
}

func pdfSpecializedFileName(fileName string) string {
	name := path.Base(strings.ReplaceAll(strings.TrimSpace(fileName), `\`, "/"))
	if name == "" || name == "." || name == "/" {
		return "document.pdf"
	}
	return name
}

func pdfSpecializedErrorMessage(message string, body []byte) string {
	if message = strings.TrimSpace(message); message != "" {
		return message
	}
	if raw := strings.TrimSpace(string(body)); raw != "" {
		return raw
	}
	return "未知错误"
}
