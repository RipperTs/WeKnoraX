package confluence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func allowLocalConfluence(t *testing.T) {
	t.Helper()
	t.Cleanup(secutils.ResetSSRFWhitelistForTest)
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,localhost")
	secutils.ResetSSRFWhitelistForTest()
}

func confluenceConfig(baseURL string) *types.DataSourceConfig {
	return &types.DataSourceConfig{
		Type:        types.ConnectorTypeConfluence,
		ResourceIDs: []string{"DOC"},
		Settings:    map[string]interface{}{"base_url": baseURL},
	}
}

func TestParseConfigNormalizesFragmentAndOptionalCredentials(t *testing.T) {
	allowLocalConfluence(t)

	ds := confluenceConfig("http://localhost:8090/#all-updates")
	cfg, err := parseConfig(ds)
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8090", cfg.BaseURL)
	assert.Empty(t, cfg.Username)
	assert.Empty(t, cfg.Password)

	ds.Credentials = map[string]interface{}{"username": "alice"}
	_, err = parseConfig(ds)
	require.Error(t, err)
	assert.ErrorIs(t, err, datasource.ErrInvalidCredentials)
}

func TestClientListSpacesPaginatesWithBasicAuth(t *testing.T) {
	allowLocalConfluence(t)

	starts := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "alice" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		starts = append(starts, r.URL.Query().Get("start"))
		response := map[string]interface{}{
			"results": []map[string]interface{}{{"key": "ENG", "name": "Engineering"}},
			"_links":  map[string]string{"next": "/rest/api/space?start=1"},
		}
		if len(starts) == 2 {
			response = map[string]interface{}{
				"results": []map[string]interface{}{{"key": "OPS", "name": "Operations"}},
				"_links":  map[string]string{},
			}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	cli := newClient(&config{BaseURL: server.URL, Username: "alice", Password: "secret"})
	spaces, err := cli.listSpaces(context.Background())
	require.NoError(t, err)
	require.Len(t, spaces, 2)
	assert.Equal(t, []string{"0", "1"}, starts)
	assert.Equal(t, "ENG", spaces[0].Key)
	assert.Equal(t, "OPS", spaces[1].Key)
}

func TestConnectorValidateSupportsAnonymousAccess(t *testing.T) {
	allowLocalConfluence(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []interface{}{},
			"_links":  map[string]string{},
		})
	}))
	defer server.Close()

	err := NewConnector().Validate(context.Background(), confluenceConfig(server.URL))
	require.NoError(t, err)
}

func TestClientDownloadRejectsOversizedAttachment(t *testing.T) {
	allowLocalConfluence(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(secutils.GetMaxFileSize()+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cli := newClient(&config{BaseURL: server.URL})
	_, err := cli.download(context.Background(), "1", "manual.pdf")
	require.ErrorIs(t, err, errAttachmentTooLarge)
	assert.Contains(t, err.Error(), "maximum download size")
}

func TestClientAbsoluteURLPreservesContextPath(t *testing.T) {
	cli := newClient(&config{BaseURL: "https://confluence.example.com/confluence"})

	assert.Equal(t,
		"https://confluence.example.com/confluence/download/attachments/1/manual.pdf",
		cli.absoluteURL("/download/attachments/1/manual.pdf"),
	)
	assert.Equal(t,
		"https://confluence.example.com/confluence/pages/viewpage.action?pageId=1",
		cli.absoluteURL("/confluence/pages/viewpage.action?pageId=1"),
	)
	assert.Equal(t,
		"https://confluence.example.com/confluence/pages/1",
		cli.absoluteURL("pages/1"),
	)
	assert.Equal(t,
		"https://confluence.example.com/confluence/download/attachments/123/manual%20final.pdf",
		cli.attachmentURL("123", "manual final.pdf"),
	)
	assert.Equal(t,
		"https://confluence.example.com/confluence/pages/viewpage.action?pageId=123",
		cli.pageURL("123"),
	)
	assert.Equal(t,
		"https://confluence.example.com/confluence/spaces/viewspace.action?key=DOC",
		cli.spaceURL("DOC"),
	)
}

func TestFetchStreamDownloadsServerAttachmentWithoutDownloadLink(t *testing.T) {
	allowLocalConfluence(t)
	downloaded := false
	mux := http.NewServeMux()
	mux.HandleFunc("/confluence/rest/api/content", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []interface{}{},
			"_links":  map[string]string{},
		})
	})
	mux.HandleFunc("/confluence/rest/api/content/search", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{{
				"id":        "20",
				"title":     "manual final.pdf",
				"version":   map[string]interface{}{"number": 1},
				"container": map[string]interface{}{"id": "10", "title": "Manuals"},
				"metadata":  map[string]interface{}{"mediaType": "application/pdf"},
			}},
			"_links": map[string]string{},
		})
	})
	mux.HandleFunc("/confluence/download/attachments/10/", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/confluence/download/attachments/10/manual final.pdf", r.URL.Path)
		downloaded = true
		_, _ = w.Write([]byte("pdf-content"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	handler := &recordingHandler{}
	next, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL+"/confluence"), nil, handler,
	)
	require.NoError(t, err)
	require.True(t, downloaded)
	require.Len(t, handler.items, 1)
	assert.Equal(t, []byte("pdf-content"), handler.items[0].Content)
	assert.Equal(t,
		server.URL+"/confluence/download/attachments/10/manual%20final.pdf",
		handler.items[0].Metadata["source_url"],
	)
	assert.Equal(t, "v1", decodeCursor(next).Items["attachment:20"])
}

type recordingHandler struct {
	items         []types.FetchedItem
	checkpoints   []*types.SyncCursor
	checkpointErr error
	checkpointAt  int
	deletionErr   error
	unhandled     map[string]bool
}

func (h *recordingHandler) Emit(_ context.Context, item types.FetchedItem) (bool, error) {
	h.items = append(h.items, item)
	if item.IsDeleted && h.deletionErr != nil {
		return false, h.deletionErr
	}
	return !h.unhandled[item.ExternalID], nil
}

func (h *recordingHandler) Checkpoint(_ context.Context, cursor *types.SyncCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	var snapshot types.SyncCursor
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	h.checkpoints = append(h.checkpoints, &snapshot)
	if h.checkpointErr != nil && (h.checkpointAt == 0 || len(h.checkpoints) == h.checkpointAt) {
		return h.checkpointErr
	}
	return nil
}

func newFetchServer(t *testing.T, pageVersion, pageStatus int) (*httptest.Server, *int) {
	t.Helper()
	pageCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/content/search", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []interface{}{},
			"_links":  map[string]string{},
		})
	})
	mux.HandleFunc("/rest/api/content/1", func(w http.ResponseWriter, _ *http.Request) {
		pageCalls++
		if pageStatus != http.StatusOK {
			http.Error(w, "page unavailable", pageStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "1",
			"title":   "Welcome",
			"space":   map[string]interface{}{"key": "DOC", "name": "Docs"},
			"version": map[string]interface{}{"number": pageVersion, "when": "2026-09-04T08:00:00Z"},
			"body": map[string]interface{}{
				"export_view": map[string]string{"value": "<p>Hello</p>"},
			},
			"_links": map[string]string{"webui": "/pages/1"},
		})
	})
	mux.HandleFunc("/rest/api/content", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{{
				"id":      "1",
				"title":   "Welcome",
				"version": map[string]interface{}{"number": pageVersion, "when": "2026-09-04T08:00:00Z"},
			}},
			"_links": map[string]string{},
		})
	})
	server := httptest.NewServer(mux)
	return server, &pageCalls
}

func TestFetchStreamRetainsFailedVersionAndEmitsDeletion(t *testing.T) {
	allowLocalConfluence(t)
	server, _ := newFetchServer(t, 2, http.StatusInternalServerError)
	defer server.Close()

	previous := encodeCursor(syncCursor{BaseURL: server.URL, Items: map[string]string{
		"page:1": "v1",
		"page:2": "v1",
	}})
	handler := &recordingHandler{}
	next, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL), previous, handler,
	)
	require.NoError(t, err)
	require.Len(t, handler.items, 2)
	assert.Equal(t, "page:1", handler.items[0].ExternalID)
	assert.Contains(t, handler.items[0].Metadata["error"], "status 500")
	assert.Equal(t, "page:2", handler.items[1].ExternalID)
	assert.True(t, handler.items[1].IsDeleted)

	state := decodeCursor(next)
	assert.Equal(t, "v1", state.Items["page:1"], "failed changed page must remain eligible for retry")
	assert.NotContains(t, state.Items, "page:2")
}

func TestFetchStreamRetainsVersionWhenIngestIsPending(t *testing.T) {
	allowLocalConfluence(t)
	server, _ := newFetchServer(t, 2, http.StatusOK)
	defer server.Close()

	previous := encodeCursor(syncCursor{BaseURL: server.URL, Items: map[string]string{"page:1": "v1"}})
	handler := &recordingHandler{unhandled: map[string]bool{"page:1": true}}
	next, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL), previous, handler,
	)
	require.NoError(t, err)

	state := decodeCursor(next)
	assert.Equal(t, "v1", state.Items["page:1"], "pending ingest must remain eligible for retry")
}

func TestFetchFullStreamRetainsDeletionWhenHandlerDefersIt(t *testing.T) {
	allowLocalConfluence(t)
	server, _ := newFetchServer(t, 1, http.StatusOK)
	defer server.Close()

	previous := encodeCursor(syncCursor{BaseURL: server.URL, Items: map[string]string{
		"page:1": "v1",
		"page:2": "v1",
	}})
	handler := &recordingHandler{unhandled: map[string]bool{"page:2": true}}
	next, err := NewConnector().FetchFullStream(
		context.Background(), confluenceConfig(server.URL), previous, handler,
	)
	require.NoError(t, err)

	state := decodeCursor(next)
	assert.Equal(t, "v1", state.Items["page:2"], "deferred deletion must remain in the cursor")
}

func TestFetchFullStreamPreservesDeletionBaselineAcrossResume(t *testing.T) {
	allowLocalConfluence(t)
	server, pageCalls := newFetchServer(t, 1, http.StatusOK)
	defer server.Close()

	baseline := encodeCursor(syncCursor{BaseURL: server.URL, Items: map[string]string{
		"page:1": "v1",
		"page:2": "v1",
	}})
	checkpointErr := errors.New("persist checkpoint failed")
	first := &recordingHandler{checkpointErr: checkpointErr, checkpointAt: 2}
	_, err := NewConnector().FetchFullStream(
		context.Background(), confluenceConfig(server.URL), baseline, first,
	)
	require.ErrorIs(t, err, checkpointErr)
	require.Len(t, first.items, 1, "full sync must fetch an unchanged page")
	require.NotEmpty(t, first.checkpoints)

	checkpoint := first.checkpoints[len(first.checkpoints)-1]
	checkpointState := decodeCursor(checkpoint)
	assert.True(t, checkpointState.FullSync)
	assert.Equal(t, "v1", checkpointState.Items["page:1"])
	assert.NotContains(t, checkpointState.Items, "page:2")
	assert.Equal(t, "v1", checkpointState.FullSyncBaseline["page:2"])

	resume := &recordingHandler{}
	next, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL), checkpoint, resume,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, *pageCalls, "resume must not download a page already saved in the checkpoint")
	require.Len(t, resume.items, 1)
	assert.Equal(t, "page:2", resume.items[0].ExternalID)
	assert.True(t, resume.items[0].IsDeleted)

	finalState := decodeCursor(next)
	assert.False(t, finalState.FullSync)
	assert.Nil(t, finalState.FullSyncBaseline)
	assert.Equal(t, map[string]string{"page:1": "v1"}, finalState.Items)
}

func TestFetchStreamKeepsDeletionInCheckpointWhenEmitFails(t *testing.T) {
	allowLocalConfluence(t)
	server, _ := newFetchServer(t, 1, http.StatusOK)
	defer server.Close()

	previous := encodeCursor(syncCursor{BaseURL: server.URL, Items: map[string]string{
		"page:1": "v1",
		"page:2": "v1",
	}})
	deleteErr := errors.New("delete failed")
	handler := &recordingHandler{deletionErr: deleteErr}
	_, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL), previous, handler,
	)
	require.ErrorIs(t, err, deleteErr)
	require.NotEmpty(t, handler.checkpoints)
	checkpointState := decodeCursor(handler.checkpoints[len(handler.checkpoints)-1])
	assert.Equal(t, "v1", checkpointState.Items["page:2"])
}

func TestFetchStreamRefreshesWhenBaseURLChanges(t *testing.T) {
	allowLocalConfluence(t)
	server, pageCalls := newFetchServer(t, 1, http.StatusOK)
	defer server.Close()

	previous := encodeCursor(syncCursor{
		BaseURL: "https://old-confluence.example.com",
		Items: map[string]string{
			"page:1": "v1",
			"page:2": "v1",
		},
	})
	handler := &recordingHandler{}
	next, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL), previous, handler,
	)
	require.NoError(t, err)

	assert.Equal(t, 1, *pageCalls, "same page ID and version on a new instance must still be fetched")
	require.Len(t, handler.items, 2)
	assert.Equal(t, "page:1", handler.items[0].ExternalID)
	assert.Equal(t, "page:2", handler.items[1].ExternalID)
	assert.True(t, handler.items[1].IsDeleted)

	state := decodeCursor(next)
	assert.Equal(t, server.URL, state.BaseURL)
	assert.Equal(t, map[string]string{"page:1": "v1"}, state.Items)
	assert.False(t, state.FullSync)
	assert.Nil(t, state.FullSyncBaseline)
}

func TestFetchStreamKeepsIntermediateItemsInBaselineAcrossAnotherURLChange(t *testing.T) {
	allowLocalConfluence(t)
	server, _ := newFetchServer(t, 1, http.StatusOK)
	defer server.Close()

	previous := encodeCursor(syncCursor{
		BaseURL:          "https://intermediate-confluence.example.com",
		Items:            map[string]string{"page:3": "v1"},
		FullSyncBaseline: map[string]string{"page:2": "v1"},
		FullSync:         true,
	})
	handler := &recordingHandler{}
	_, err := NewConnector().FetchStream(
		context.Background(), confluenceConfig(server.URL), previous, handler,
	)
	require.NoError(t, err)

	deletedIDs := make([]string, 0, 2)
	for _, item := range handler.items {
		if item.IsDeleted {
			deletedIDs = append(deletedIDs, item.ExternalID)
		}
	}
	assert.ElementsMatch(t, []string{"page:2", "page:3"}, deletedIDs)
}
