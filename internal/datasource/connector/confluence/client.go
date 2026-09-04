package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
)

type client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

func newClient(cfg *config) *client {
	return &client{
		baseURL:    cfg.BaseURL,
		username:   cfg.Username,
		password:   cfg.Password,
		httpClient: datasource.NewConnectorHTTPClient(requestTimeout),
	}
}

func (c *client) ping(ctx context.Context) error {
	var result spaceList
	if err := c.getJSON(ctx, "/rest/api/space", url.Values{"limit": {"1"}}, &result); err != nil {
		return fmt.Errorf("confluence connection failed: %w", err)
	}
	return nil
}

func (c *client) listSpaces(ctx context.Context) ([]space, error) {
	var out []space
	start := 0
	for {
		var result spaceList
		query := url.Values{
			"start":  {strconv.Itoa(start)},
			"limit":  {strconv.Itoa(defaultPageSize)},
			"expand": {"description.plain"},
		}
		if err := c.getJSON(ctx, "/rest/api/space", query, &result); err != nil {
			return nil, err
		}
		out = append(out, result.Results...)
		if result.Links.Next == "" || len(result.Results) == 0 {
			return out, nil
		}
		start += len(result.Results)
	}
}

func (c *client) listPages(ctx context.Context, spaceKey string) ([]page, error) {
	var out []page
	start := 0
	for {
		var result pageList
		query := url.Values{
			"spaceKey": {spaceKey},
			"type":     {"page"},
			"status":   {"current"},
			"start":    {strconv.Itoa(start)},
			"limit":    {strconv.Itoa(defaultPageSize)},
			"expand":   {"space,version,ancestors"},
		}
		if err := c.getJSON(ctx, "/rest/api/content", query, &result); err != nil {
			return nil, err
		}
		out = append(out, result.Results...)
		if result.Links.Next == "" || len(result.Results) == 0 {
			return out, nil
		}
		start += len(result.Results)
	}
}

func (c *client) getPage(ctx context.Context, pageID string) (*page, error) {
	var result page
	query := url.Values{"expand": {"space,version,ancestors,body.export_view"}}
	if err := c.getJSON(ctx, "/rest/api/content/"+url.PathEscape(pageID), query, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *client) listAttachments(ctx context.Context, spaceKey string) ([]attachment, error) {
	var out []attachment
	start := 0
	for {
		var result attachmentList
		query := url.Values{
			"cql":    {fmt.Sprintf(`type=attachment and space="%s"`, escapeCQL(spaceKey))},
			"start":  {strconv.Itoa(start)},
			"limit":  {strconv.Itoa(defaultPageSize)},
			"expand": {"space,container,version,metadata"},
		}
		if err := c.getJSON(ctx, "/rest/api/content/search", query, &result); err != nil {
			return nil, err
		}
		out = append(out, result.Results...)
		if result.Links.Next == "" || len(result.Results) == 0 {
			return out, nil
		}
		start += len(result.Results)
	}
}

func (c *client) download(ctx context.Context, containerID, fileName string) ([]byte, error) {
	req, err := c.newRequest(ctx, c.attachmentURL(containerID, fileName))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute Confluence attachment request: %w", err)
	}
	defer resp.Body.Close()
	if err := confluenceStatusError(resp); err != nil {
		return nil, err
	}
	if resp.ContentLength > maxAttachmentDownloadBytes {
		return nil, fmt.Errorf(
			"Confluence attachment exceeds maximum download size (%d MB)",
			maxAttachmentDownloadBytes/(1024*1024),
		)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Confluence attachment: %w", err)
	}
	if int64(len(data)) > maxAttachmentDownloadBytes {
		return nil, fmt.Errorf(
			"Confluence attachment exceeds maximum download size (%d MB)",
			maxAttachmentDownloadBytes/(1024*1024),
		)
	}
	return data, nil
}

func (c *client) attachmentURL(containerID, fileName string) string {
	return c.baseURL + "/download/attachments/" +
		url.PathEscape(containerID) + "/" + url.PathEscape(fileName)
}

func (c *client) pageURL(pageID string) string {
	return c.baseURL + "/pages/viewpage.action?pageId=" + url.QueryEscape(pageID)
}

func (c *client) spaceURL(spaceKey string) string {
	return c.baseURL + "/spaces/viewspace.action?key=" + url.QueryEscape(spaceKey)
}

func (c *client) getJSON(ctx context.Context, endpoint string, query url.Values, result interface{}) error {
	requestURL := c.baseURL + endpoint
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	req, err := c.newRequest(ctx, requestURL)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute Confluence request: %w", err)
	}
	defer resp.Body.Close()
	if err := confluenceStatusError(resp); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decode Confluence response: %w", err)
	}
	return nil
}

func (c *client) newRequest(ctx context.Context, requestURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create Confluence request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "WeKnora-Confluence-Connector/1.0")
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	return req, nil
}

func (c *client) absoluteURL(link string) string {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return link
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	base, err := url.Parse(c.baseURL + "/")
	if err != nil {
		return link
	}
	if parsed.Host != "" {
		return base.ResolveReference(parsed).String()
	}
	contextPath := strings.TrimRight(base.Path, "/")
	if strings.HasPrefix(parsed.Path, "/") && contextPath != "" &&
		parsed.Path != contextPath && !strings.HasPrefix(parsed.Path, contextPath+"/") {
		parsed.Path = contextPath + parsed.Path
		parsed.RawPath = ""
	}
	return base.ResolveReference(parsed).String()
}

func confluenceStatusError(resp *http.Response) error {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: Confluence returned status %d", datasource.ErrInvalidCredentials, resp.StatusCode)
	}
	return fmt.Errorf("Confluence API returned status %d", resp.StatusCode)
}

func escapeCQL(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
