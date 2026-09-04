// Package confluence implements the Confluence Server data source connector.
package confluence

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	defaultPageSize            = 100
	requestTimeout             = 30 * time.Second
	maxAttachmentDownloadBytes = 100 * 1024 * 1024
)

// config contains the non-secret Confluence address and optional Basic Auth
// credentials used by older Confluence Server installations.
type config struct {
	BaseURL  string
	Username string
	Password string
}

func parseConfig(ds *types.DataSourceConfig) (*config, error) {
	if ds == nil {
		return nil, datasource.ErrInvalidConfig
	}

	baseURL, _ := ds.Settings["base_url"].(string)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("%w: settings.base_url is required", datasource.ErrInvalidConfig)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%w: base_url must be an absolute HTTP or HTTPS URL", datasource.ErrInvalidConfig)
	}
	if parsed.User != nil || parsed.RawQuery != "" {
		return nil, fmt.Errorf("%w: base_url must not contain user information or query parameters", datasource.ErrInvalidConfig)
	}
	if err := datasource.ValidateConnectorBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("%w (for private Confluence deployments, add the host to SSRF_WHITELIST)", err)
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	baseURL = strings.TrimRight(parsed.String(), "/")

	username, _ := ds.Credentials["username"].(string)
	password, _ := ds.Credentials["password"].(string)
	username = strings.TrimSpace(username)
	if (username == "") != (password == "") {
		return nil, fmt.Errorf("%w: username and password must be provided together", datasource.ErrInvalidCredentials)
	}

	return &config{BaseURL: baseURL, Username: username, Password: password}, nil
}

type apiLinks struct {
	Base     string `json:"base"`
	WebUI    string `json:"webui"`
	Download string `json:"download"`
	Next     string `json:"next"`
}

type plainBody struct {
	Value string `json:"value"`
}

type space struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description struct {
		Plain plainBody `json:"plain"`
	} `json:"description"`
	Links apiLinks `json:"_links"`
}

type version struct {
	Number int       `json:"number"`
	When   time.Time `json:"when"`
}

type ancestor struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type page struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Space     space      `json:"space"`
	Version   version    `json:"version"`
	Ancestors []ancestor `json:"ancestors"`
	Body      struct {
		ExportView plainBody `json:"export_view"`
	} `json:"body"`
	Links apiLinks `json:"_links"`
}

type attachment struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Space     space   `json:"space"`
	Container page    `json:"container"`
	Version   version `json:"version"`
	Metadata  struct {
		MediaType string `json:"mediaType"`
	} `json:"metadata"`
	Links apiLinks `json:"_links"`
}

type spaceList struct {
	Results []space  `json:"results"`
	Links   apiLinks `json:"_links"`
}

type pageList struct {
	Results []page   `json:"results"`
	Links   apiLinks `json:"_links"`
}

type attachmentList struct {
	Results []attachment `json:"results"`
	Links   apiLinks     `json:"_links"`
}

type syncCursor struct {
	Items            map[string]string `json:"items"`
	FullSyncBaseline map[string]string `json:"full_sync_baseline,omitempty"`
	FullSync         bool              `json:"full_sync,omitempty"`
}
