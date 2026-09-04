package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/PuerkitoBio/goquery"
	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	pageIDPrefix            = "page:"
	attachmentIDPrefix      = "attachment:"
	checkpointItemInterval  = 50
	checkpointTimeInterval  = 30 * time.Second
	confluenceResourceSpace = "confluence_space"
)

var _ datasource.FullSyncStreamingConnector = (*Connector)(nil)

// Connector synchronizes Confluence spaces, pages, and supported attachments.
type Connector struct{}

func NewConnector() *Connector { return &Connector{} }

func (c *Connector) Type() string { return types.ConnectorTypeConfluence }

func (c *Connector) Validate(ctx context.Context, ds *types.DataSourceConfig) error {
	cfg, err := parseConfig(ds)
	if err != nil {
		return err
	}
	return newClient(cfg).ping(ctx)
}

func (c *Connector) ListResources(
	ctx context.Context, ds *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	if parentID != "" {
		return []types.Resource{}, nil
	}
	cfg, err := parseConfig(ds)
	if err != nil {
		return nil, err
	}
	cli := newClient(cfg)
	spaces, err := cli.listSpaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Confluence spaces: %w", err)
	}

	resources := make([]types.Resource, 0, len(spaces))
	for _, item := range spaces {
		if strings.TrimSpace(item.Key) == "" {
			continue
		}
		resources = append(resources, types.Resource{
			ExternalID:  item.Key,
			Name:        item.Name,
			Type:        confluenceResourceSpace,
			Description: strings.TrimSpace(item.Description.Plain.Value),
			URL:         cli.spaceURL(item.Key),
			Metadata: map[string]interface{}{
				"space_key":  item.Key,
				"space_type": item.Type,
			},
		})
	}
	sort.Slice(resources, func(i, j int) bool {
		return strings.ToLower(resources[i].Name) < strings.ToLower(resources[j].Name)
	})
	return resources, nil
}

func (c *Connector) ResolveResourceAncestors(
	context.Context, *types.DataSourceConfig, []string,
) ([]string, error) {
	return []string{}, nil
}

func (c *Connector) FetchAll(
	ctx context.Context, ds *types.DataSourceConfig, resourceIDs []string,
) ([]types.FetchedItem, error) {
	if ds == nil {
		return nil, datasource.ErrInvalidConfig
	}
	config := *ds
	config.ResourceIDs = resourceIDs
	handler := &collectHandler{}
	if _, err := c.FetchStream(ctx, &config, nil, handler); err != nil {
		return nil, err
	}
	return handler.items, nil
}

func (c *Connector) FetchIncremental(
	ctx context.Context, ds *types.DataSourceConfig, cursor *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	handler := &collectHandler{}
	next, err := c.FetchStream(ctx, ds, cursor, handler)
	if err != nil {
		return nil, nil, err
	}
	return handler.items, next, nil
}

// FetchStream enumerates lightweight page and attachment metadata, then only
// downloads items whose Confluence version changed. Content is emitted one item
// at a time so large spaces do not accumulate document bodies in memory.
func (c *Connector) FetchStream(
	ctx context.Context,
	ds *types.DataSourceConfig,
	cursor *types.SyncCursor,
	handler datasource.StreamHandler,
) (*types.SyncCursor, error) {
	return c.fetchStream(ctx, ds, cursor, handler, false)
}

// FetchFullStream refreshes all current content while retaining the prior
// cursor as the deletion baseline. Its checkpoints are consumed by FetchStream
// on retry so a large full sync remains resumable.
func (c *Connector) FetchFullStream(
	ctx context.Context,
	ds *types.DataSourceConfig,
	baseline *types.SyncCursor,
	handler datasource.StreamHandler,
) (*types.SyncCursor, error) {
	return c.fetchStream(ctx, ds, baseline, handler, true)
}

func (c *Connector) fetchStream(
	ctx context.Context,
	ds *types.DataSourceConfig,
	cursor *types.SyncCursor,
	handler datasource.StreamHandler,
	forceFull bool,
) (*types.SyncCursor, error) {
	if ds == nil {
		return nil, fmt.Errorf("%w: no Confluence spaces configured", datasource.ErrInvalidConfig)
	}
	spaceKeys := uniqueResourceIDs(ds.ResourceIDs)
	if len(spaceKeys) == 0 {
		return nil, fmt.Errorf("%w: no Confluence spaces configured", datasource.ErrInvalidConfig)
	}
	cfg, err := parseConfig(ds)
	if err != nil {
		return nil, err
	}
	cli := newClient(cfg)
	previous := decodeCursor(cursor)
	sourceChanged := previous.BaseURL != "" && previous.BaseURL != cfg.BaseURL
	fullRefresh := forceFull || sourceChanged
	comparison := previous.Items
	deletionBaseline := previous.Items
	next := syncCursor{BaseURL: cfg.BaseURL, Items: make(map[string]string, len(previous.Items))}
	if fullRefresh {
		comparison = map[string]string{}
		next.FullSync = true
		baselineItems := previous.Items
		if previous.FullSync {
			baselineItems = previous.FullSyncBaseline
		}
		next.FullSyncBaseline = cloneSignatures(baselineItems)
		if previous.FullSync {
			// A new full refresh can replace an interrupted one. Items already
			// ingested by the interrupted refresh must remain in the deletion
			// baseline so they cannot become orphaned.
			for id, signature := range previous.Items {
				next.FullSyncBaseline[id] = signature
			}
		}
		deletionBaseline = next.FullSyncBaseline
	} else if previous.FullSync {
		next.FullSync = true
		next.FullSyncBaseline = cloneSignatures(previous.FullSyncBaseline)
		deletionBaseline = next.FullSyncBaseline
	}
	if !fullRefresh {
		for id, signature := range previous.Items {
			next.Items[id] = signature
		}
	}
	current := make(map[string]struct{})
	fullSyncHasPendingItems := false

	processed := 0
	lastCheckpoint := time.Now()
	checkpoint := func(force bool) error {
		if !force && processed%checkpointItemInterval != 0 && time.Since(lastCheckpoint) < checkpointTimeInterval {
			return nil
		}
		if err := handler.Checkpoint(ctx, encodeCursor(next)); err != nil {
			return err
		}
		lastCheckpoint = time.Now()
		return nil
	}
	if fullRefresh {
		// Persist the full-sync marker before the first remote request. If the
		// traversal fails before its first page boundary, the service retry still
		// knows to continue the full refresh instead of falling back to incremental.
		if err := checkpoint(true); err != nil {
			return nil, err
		}
	}

	for _, spaceKey := range spaceKeys {
		pages, err := cli.listPages(ctx, spaceKey)
		if err != nil {
			return nil, fmt.Errorf("list Confluence pages in space %s: %w", spaceKey, err)
		}
		for _, pageMeta := range pages {
			externalID := pageIDPrefix + pageMeta.ID
			signature := "v" + strconv.Itoa(pageMeta.Version.Number)
			current[externalID] = struct{}{}
			if comparison[externalID] != signature {
				pageDetail, fetchErr := cli.getPage(ctx, pageMeta.ID)
				if fetchErr != nil {
					if next.FullSync {
						fullSyncHasPendingItems = true
					}
					if _, emitErr := handler.Emit(
						ctx, failedItem(externalID, pageMeta.Title, spaceKey, fetchErr),
					); emitErr != nil {
						return nil, emitErr
					}
				} else {
					item, buildErr := buildPageItem(cli, pageDetail, spaceKey)
					if buildErr != nil {
						if next.FullSync {
							fullSyncHasPendingItems = true
						}
						if _, emitErr := handler.Emit(
							ctx, failedItem(externalID, pageMeta.Title, spaceKey, buildErr),
						); emitErr != nil {
							return nil, emitErr
						}
					} else {
						handled, emitErr := handler.Emit(ctx, item)
						if emitErr != nil {
							return nil, emitErr
						}
						if handled {
							next.Items[externalID] = signature
						} else if next.FullSync {
							fullSyncHasPendingItems = true
						}
					}
				}
			}
			processed++
			if err := checkpoint(false); err != nil {
				return nil, err
			}
		}

		attachments, err := cli.listAttachments(ctx, spaceKey)
		if err != nil {
			return nil, fmt.Errorf("list Confluence attachments in space %s: %w", spaceKey, err)
		}
		for _, attachmentMeta := range attachments {
			if !supportedAttachment(attachmentMeta.Title) {
				continue
			}
			externalID := attachmentIDPrefix + attachmentMeta.ID
			signature := "v" + strconv.Itoa(attachmentMeta.Version.Number)
			current[externalID] = struct{}{}
			if comparison[externalID] != signature {
				content, fetchErr := cli.download(ctx, attachmentMeta.Container.ID, attachmentMeta.Title)
				if fetchErr != nil {
					item := failedItem(externalID, attachmentMeta.Title, spaceKey, fetchErr)
					if errors.Is(fetchErr, errAttachmentTooLarge) {
						item = types.FetchedItem{
							ExternalID: externalID,
							Title:      attachmentMeta.Title,
							Metadata:   map[string]string{"channel": types.ChannelConfluence},
						}
					}
					handled, emitErr := handler.Emit(ctx, item)
					if emitErr != nil {
						return nil, emitErr
					}
					if handled && errors.Is(fetchErr, errAttachmentTooLarge) {
						next.Items[externalID] = signature
					} else if next.FullSync {
						fullSyncHasPendingItems = true
					}
				} else {
					item := buildAttachmentItem(cli, attachmentMeta, spaceKey, content)
					handled, emitErr := handler.Emit(ctx, item)
					if emitErr != nil {
						return nil, emitErr
					}
					if handled {
						next.Items[externalID] = signature
					} else if next.FullSync {
						fullSyncHasPendingItems = true
					}
				}
			}
			processed++
			if err := checkpoint(false); err != nil {
				return nil, err
			}
		}
		if err := checkpoint(true); err != nil {
			return nil, err
		}
	}

	for externalID := range deletionBaseline {
		if _, exists := current[externalID]; exists {
			continue
		}
		handled, err := handler.Emit(ctx, types.FetchedItem{
			ExternalID: externalID,
			IsDeleted:  true,
			Metadata:   map[string]string{"channel": types.ChannelConfluence},
		})
		if err != nil {
			return nil, err
		}
		if handled {
			delete(next.Items, externalID)
			if next.FullSync {
				delete(next.FullSyncBaseline, externalID)
			}
		} else {
			next.Items[externalID] = deletionBaseline[externalID]
		}
	}
	if !fullSyncHasPendingItems {
		next.FullSync = false
		next.FullSyncBaseline = nil
	}

	return encodeCursor(next), nil
}

type collectHandler struct {
	items []types.FetchedItem
}

func (h *collectHandler) Emit(_ context.Context, item types.FetchedItem) (bool, error) {
	h.items = append(h.items, item)
	return true, nil
}

func (h *collectHandler) Checkpoint(context.Context, *types.SyncCursor) error { return nil }

func buildPageItem(cli *client, item *page, resourceID string) (types.FetchedItem, error) {
	html, err := absolutizeHTMLLinks(cli, item.Body.ExportView.Value)
	if err != nil {
		return types.FetchedItem{}, fmt.Errorf("normalize Confluence page links: %w", err)
	}
	markdown, err := htmltomd.ConvertString(html)
	if err != nil {
		return types.FetchedItem{}, fmt.Errorf("convert Confluence page to Markdown: %w", err)
	}
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = "Confluence page " + item.ID
	}
	content := "# " + strings.ReplaceAll(title, "\n", " ")
	if strings.TrimSpace(markdown) != "" {
		content += "\n\n" + strings.TrimSpace(markdown)
	}
	ancestorTitles := make([]string, 0, len(item.Ancestors))
	for _, parent := range item.Ancestors {
		if parent.Title != "" {
			ancestorTitles = append(ancestorTitles, parent.Title)
		}
	}
	pageURL := cli.pageURL(item.ID)
	return types.FetchedItem{
		ExternalID:       pageIDPrefix + item.ID,
		Title:            title,
		Content:          []byte(content),
		ContentType:      "text/markdown",
		FileName:         sanitizeFileName(title, item.ID) + ".md",
		URL:              pageURL,
		UpdatedAt:        item.Version.When,
		SourceResourceID: resourceID,
		Metadata: map[string]string{
			"channel":       types.ChannelConfluence,
			"source_type":   types.ConnectorTypeConfluence,
			"space_key":     resourceID,
			"space_name":    item.Space.Name,
			"page_id":       item.ID,
			"page_version":  strconv.Itoa(item.Version.Number),
			"ancestor_path": strings.Join(ancestorTitles, " / "),
			"source_url":    pageURL,
		},
	}, nil
}

func buildAttachmentItem(cli *client, item attachment, resourceID string, content []byte) types.FetchedItem {
	fileName := sanitizeAttachmentFileName(item.Title, item.ID)
	sourceURL := cli.attachmentURL(item.Container.ID, item.Title)
	return types.FetchedItem{
		ExternalID:       attachmentIDPrefix + item.ID,
		Title:            item.Title,
		Content:          content,
		ContentType:      item.Metadata.MediaType,
		FileName:         fileName,
		URL:              sourceURL,
		UpdatedAt:        item.Version.When,
		SourceResourceID: resourceID,
		Metadata: map[string]string{
			"channel":            types.ChannelConfluence,
			"source_type":        types.ConnectorTypeConfluence,
			"space_key":          resourceID,
			"attachment_id":      item.ID,
			"attachment_version": strconv.Itoa(item.Version.Number),
			"parent_page_id":     item.Container.ID,
			"parent_page_title":  item.Container.Title,
			"source_url":         sourceURL,
		},
	}
}

func failedItem(externalID, title, resourceID string, err error) types.FetchedItem {
	return types.FetchedItem{
		ExternalID:       externalID,
		Title:            title,
		SourceResourceID: resourceID,
		Metadata: map[string]string{
			"channel": types.ChannelConfluence,
			"error":   err.Error(),
		},
	}
}

func absolutizeHTMLLinks(cli *client, html string) (string, error) {
	if strings.TrimSpace(html) == "" {
		return "", nil
	}
	document, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}
	for _, attribute := range []string{"href", "src"} {
		document.Find("[" + attribute + "]").Each(func(_ int, selection *goquery.Selection) {
			value, exists := selection.Attr(attribute)
			if !exists || strings.TrimSpace(value) == "" || strings.HasPrefix(value, "#") {
				return
			}
			parsed, parseErr := url.Parse(value)
			if parseErr != nil || parsed.IsAbs() || parsed.Scheme != "" {
				return
			}
			selection.SetAttr(attribute, cli.absoluteURL(value))
		})
	}
	return document.Find("body").Html()
}

func decodeCursor(cursor *types.SyncCursor) syncCursor {
	result := syncCursor{Items: map[string]string{}}
	if cursor == nil || cursor.ConnectorCursor == nil {
		return result
	}
	data, err := json.Marshal(cursor.ConnectorCursor)
	if err != nil {
		return result
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Items == nil {
		result.Items = map[string]string{}
	}
	return result
}

func encodeCursor(cursor syncCursor) *types.SyncCursor {
	data, _ := json.Marshal(cursor)
	connectorCursor := make(map[string]interface{})
	_ = json.Unmarshal(data, &connectorCursor)
	return &types.SyncCursor{
		LastSyncTime:    time.Now().UTC(),
		ConnectorCursor: connectorCursor,
	}
}

func cloneSignatures(items map[string]string) map[string]string {
	result := make(map[string]string, len(items))
	for id, signature := range items {
		result[id] = signature
	}
	return result
}

func uniqueResourceIDs(resourceIDs []string) []string {
	seen := make(map[string]struct{}, len(resourceIDs))
	result := make([]string, 0, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		resourceID = strings.TrimSpace(resourceID)
		if resourceID == "" {
			continue
		}
		if _, exists := seen[resourceID]; exists {
			continue
		}
		seen[resourceID] = struct{}{}
		result = append(result, resourceID)
	}
	return result
}

func sanitizeFileName(name, fallbackID string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name))
	if name == "" {
		name = "confluence-" + fallbackID
	}
	for len(name) > 180 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

func sanitizeAttachmentFileName(name, fallbackID string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return sanitizeFileName(base, fallbackID) + ext
}

func supportedAttachment(name string) bool {
	_, exists := supportedAttachmentExtensions[strings.ToLower(filepath.Ext(name))]
	return exists
}

var supportedAttachmentExtensions = map[string]struct{}{
	".pdf": {}, ".txt": {}, ".docx": {}, ".doc": {}, ".epub": {},
	".html": {}, ".htm": {}, ".mhtml": {}, ".md": {}, ".markdown": {},
	".csv": {}, ".xlsx": {}, ".xls": {}, ".pptx": {}, ".ppt": {}, ".json": {},
	".mp3": {}, ".wav": {}, ".m4a": {}, ".flac": {}, ".ogg": {},
}
