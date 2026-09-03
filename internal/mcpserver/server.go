// Package mcpserver exposes a deliberately narrow, read-only MCP surface over
// the canonical archive's existing search implementations.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/embeddings"
)

const searchInputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": {
      "type": "string",
      "minLength": 1,
      "maxLength": 500,
      "description": "Text or meaning to search for. FTS accepts PostgreSQL web-style search syntax including quoted phrases and exclusions."
    },
    "mode": {
      "type": "string",
      "enum": ["fts", "semantic"],
      "default": "fts",
      "description": "fts searches locally with PostgreSQL. semantic sends only this query to the configured OpenAI embeddings endpoint, then compares it with the local derived vector index."
    },
    "channel_id": {
      "type": "string",
      "description": "Optional OpenConvo channel UUID. Result objects include channel IDs for follow-up searches."
    },
    "author": {
      "type": "string",
      "maxLength": 200,
      "description": "Optional case-insensitive substring of the username or display name."
    },
    "after": {
      "type": "string",
      "description": "Optional inclusive lower date bound as YYYY-MM-DD or an RFC3339 timestamp."
    },
    "before": {
      "type": "string",
      "description": "Optional exclusive upper date bound as YYYY-MM-DD or an RFC3339 timestamp."
    },
    "has_attachment": {
      "type": "boolean",
      "description": "When present, require messages to have attachments (true) or have none (false)."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 100,
      "default": 25,
      "description": "Maximum results in this page."
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "maximum": 100000,
      "default": 0,
      "description": "Result offset for pagination. Use next_offset from a prior response."
    }
  },
  "required": ["query"]
}`

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SearchAPI is the one operation the MCP boundary may perform. The keyword
// archive store and the optional semantic service both satisfy it.
type SearchAPI interface {
	SearchMessages(context.Context, archive.SearchParams) (archive.SearchPage, error)
}

// Deps are the read-only search implementations exposed by the MCP server.
type Deps struct {
	Keyword  SearchAPI
	Semantic SearchAPI
	Logger   *slog.Logger
}

type searchInput struct {
	Query         string `json:"query"`
	Mode          string `json:"mode"`
	ChannelID     string `json:"channel_id,omitempty"`
	Author        string `json:"author,omitempty"`
	After         string `json:"after,omitempty"`
	Before        string `json:"before,omitempty"`
	HasAttachment *bool  `json:"has_attachment,omitempty"`
	Limit         int    `json:"limit"`
	Offset        int    `json:"offset"`
}

// SearchOutput is the structured result returned to MCP clients. It omits
// actor UUIDs and avatar URLs because they do not help answer a search query.
type SearchOutput struct {
	Results    []SearchResult `json:"results"`
	HasMore    bool           `json:"has_more"`
	NextOffset int            `json:"next_offset,omitempty"`
}

type SearchResult struct {
	MessageID       string       `json:"message_id"`
	ChannelID       string       `json:"channel_id"`
	ChannelName     string       `json:"channel_name"`
	CommunityName   string       `json:"community_name"`
	Author          *SearchActor `json:"author,omitempty"`
	SourceCreatedAt string       `json:"source_created_at"`
	Excerpt         string       `json:"excerpt"`
	HasAttachment   bool         `json:"has_attachment"`
}

type SearchActor struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

// New constructs a server with exactly one read-only tool and no resources,
// prompts, sampling, or network listener.
func New(deps Deps, serverVersion string) *mcp.Server {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "openconvo",
		Title:   "OpenConvo Archive",
		Version: serverVersion,
	}, &mcp.ServerOptions{Logger: deps.Logger.With("component", "mcp")})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_messages",
		Title:       "Search archived messages",
		Description: "Search live, non-deleted archived messages using local PostgreSQL full-text search or the optional semantic index. Supports the same channel, author, date, attachment, and pagination filters as OpenConvo's search page.",
		InputSchema: json.RawMessage(searchInputSchema),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, searchHandler(deps))

	return server
}

func searchHandler(deps Deps) mcp.ToolHandlerFor[searchInput, SearchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, SearchOutput, error) {
		params, mode, err := searchParams(input)
		if err != nil {
			return nil, SearchOutput{}, err
		}

		searcher := deps.Keyword
		if mode == "semantic" {
			searcher = deps.Semantic
		}
		if searcher == nil {
			return nil, SearchOutput{}, fmt.Errorf("%s search is unavailable", mode)
		}

		page, err := searcher.SearchMessages(ctx, params)
		if err != nil {
			return nil, SearchOutput{}, searchError(deps.Logger, mode, err)
		}
		output := SearchOutput{
			Results: make([]SearchResult, 0, len(page.Results)),
			HasMore: page.HasMore,
		}
		if page.HasMore {
			output.NextOffset = params.Offset + len(page.Results)
		}
		for _, result := range page.Results {
			out := SearchResult{
				MessageID:       result.MessageID,
				ChannelID:       result.ChannelID,
				ChannelName:     result.ChannelName,
				CommunityName:   result.CommunityName,
				SourceCreatedAt: result.SourceCreatedAt.UTC().Format(time.RFC3339Nano),
				Excerpt:         result.Excerpt,
				HasAttachment:   result.HasAttachment,
			}
			if result.Actor != nil {
				out.Author = &SearchActor{
					Username:    result.Actor.Username,
					DisplayName: result.Actor.DisplayName,
					IsBot:       result.Actor.IsBot,
				}
			}
			output.Results = append(output.Results, out)
		}
		return nil, output, nil
	}
}

func searchParams(input searchInput) (archive.SearchParams, string, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" || utf8.RuneCountInString(query) > 500 {
		return archive.SearchParams{}, "", errors.New("query is required and must be at most 500 characters")
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode == "" {
		mode = "fts"
	}
	if mode != "fts" && mode != "semantic" {
		return archive.SearchParams{}, "", errors.New("mode must be fts or semantic")
	}
	channelID := strings.TrimSpace(input.ChannelID)
	if channelID != "" && !uuidPattern.MatchString(channelID) {
		return archive.SearchParams{}, "", errors.New("channel_id must be a UUID")
	}
	author := strings.TrimSpace(input.Author)
	if utf8.RuneCountInString(author) > 200 {
		return archive.SearchParams{}, "", errors.New("author must be at most 200 characters")
	}
	after, err := parseTimeBound("after", input.After)
	if err != nil {
		return archive.SearchParams{}, "", err
	}
	before, err := parseTimeBound("before", input.Before)
	if err != nil {
		return archive.SearchParams{}, "", err
	}
	if after != nil && before != nil && !after.Before(*before) {
		return archive.SearchParams{}, "", errors.New("after must be earlier than before")
	}
	limit := input.Limit
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > 100 {
		return archive.SearchParams{}, "", errors.New("limit must be between 1 and 100")
	}
	if input.Offset < 0 || input.Offset > 100000 {
		return archive.SearchParams{}, "", errors.New("offset must be between 0 and 100000")
	}
	return archive.SearchParams{
		Query:         query,
		ChannelID:     channelID,
		Author:        author,
		After:         after,
		Before:        before,
		HasAttachment: input.HasAttachment,
		Limit:         limit,
		Offset:        input.Offset,
	}, mode, nil
}

func parseTimeBound(name, value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil {
		return nil, fmt.Errorf("%s must be YYYY-MM-DD or RFC3339", name)
	}
	return &parsed, nil
}

func searchError(logger *slog.Logger, mode string, err error) error {
	switch {
	case errors.Is(err, embeddings.ErrDisabled):
		return errors.New("semantic search is disabled; enable message embeddings in OpenConvo Settings")
	case errors.Is(err, embeddings.ErrNotConfigured):
		return errors.New("semantic search requires OPENAI_API_KEY")
	case errors.Is(err, embeddings.ErrNotReady):
		return errors.New("semantic index is still building")
	case errors.Is(err, embeddings.ErrProvider):
		logger.Warn("semantic search provider failed", "error", err)
		return errors.New("semantic search provider failed")
	default:
		logger.Error("search messages failed", "mode", mode, "error", err)
		return errors.New("search failed")
	}
}
