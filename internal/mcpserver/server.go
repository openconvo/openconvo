// Package mcpserver exposes a deliberately narrow, read-only MCP surface over
// the canonical archive's existing search and reading queries.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
      "description": "Words or meaning to search for. fts accepts web-search syntax: \"quoted phrases\", or between alternatives, and -word to exclude."
    },
    "mode": {
      "type": "string",
      "enum": ["fts", "semantic"],
      "default": "fts",
      "description": "fts searches locally with PostgreSQL. semantic sends only this query to the configured OpenAI embeddings endpoint, then compares it with the local derived vector index."
    },
    "channel_id": {
      "type": "string",
      "description": "Optional OpenConvo channel UUID from list_channels or a result. A thread is a channel of its own."
    },
    "author": {
      "type": "string",
      "maxLength": 200,
      "description": "Optional case-insensitive substring of the username or display name."
    },
    "after": {
      "type": "string",
      "description": "Optional inclusive lower bound: YYYY-MM-DD (midnight UTC) or an RFC3339 timestamp such as 2026-09-01T00:00:00+02:00."
    },
    "before": {
      "type": "string",
      "description": "Optional exclusive upper bound: YYYY-MM-DD (midnight UTC) or an RFC3339 timestamp such as 2026-09-02T00:00:00+02:00."
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

// SearchAPI is satisfied by the archive's keyword search and by the optional
// semantic index.
type SearchAPI interface {
	SearchMessages(context.Context, archive.SearchParams) (archive.SearchPage, error)
}

// Archive is the read-only archive surface behind the tools. *archive.Store
// satisfies it.
type Archive interface {
	SearchAPI
	GetArchiveChannel(ctx context.Context, channelID string) (archive.ArchiveChannel, bool, error)
	ListArchiveChannels(ctx context.Context) ([]archive.ArchiveChannel, error)
	GetArchiveMessages(ctx context.Context, ids []string) ([]archive.ArchiveMessage, error)
	GetMessageContext(ctx context.Context, messageID string, beforeCount, afterCount int) (archive.MessageContext, bool, error)
}

// Deps are the read-only implementations exposed by the MCP server.
type Deps struct {
	Archive  Archive
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

// SearchOutput is the structured result returned to MCP clients.
type SearchOutput struct {
	Results    []SearchResult `json:"results"`
	HasMore    bool           `json:"has_more"`
	NextOffset int            `json:"next_offset,omitempty"`
}

// SearchResult is one hit: the message and where it was found. Excerpt is
// set in fts mode only, and Distance in semantic mode only.
type SearchResult struct {
	Message
	ChannelID     string   `json:"channel_id"`
	ChannelName   string   `json:"channel_name"`
	CommunityName string   `json:"community_name"`
	Excerpt       string   `json:"excerpt,omitempty"`
	HasAttachment bool     `json:"has_attachment"`
	Distance      *float64 `json:"distance,omitempty"`
}

// New constructs a server with read-only tools and no resources, prompts,
// sampling, or network listener.
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
		Description: searchDescription,
		InputSchema: json.RawMessage(searchInputSchema),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, searchHandler(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_message_context",
		Title:       "Read a message in context",
		Description: contextDescription,
		InputSchema: json.RawMessage(contextInputSchema),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, contextHandler(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_channels",
		Title:       "List archived channels",
		Description: channelsDescription,
		InputSchema: json.RawMessage(channelsInputSchema),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, channelsHandler(deps))

	return server
}

const searchDescription = "Search live, non-deleted archived messages. " +
	"mode fts (the default) runs full-text search locally and matches whole words, ignoring letter case and accents: " +
	"cafe finds café, but bake does not find baked, so list the word forms you need with or. " +
	"It has no wildcards, ignores emoji and punctuation, and matches a web address only as a whole host such as www.example.com. " +
	"mode semantic matches meaning across wording and languages, and each result carries a distance: lower is closer, " +
	"and a best result that is still distant means nothing relevant was found. " +
	"Each result carries the message's text in content, cut at 2000 characters with … and content_truncated; " +
	"get_message_context returns a message in full with the conversation around it. " +
	"In fts mode, excerpt shows the matching passage with matches wrapped in <mark></mark>, " +
	"starting or ending with … where it leaves text out. " +
	"A date without a time means midnight UTC; to cover a local day, pass RFC3339 timestamps with the offset. " +
	"list_channels gives the IDs for channel_id. " +
	"Supports the same channel, author, date, attachment, and pagination filters as OpenConvo's search page."

func searchHandler(deps Deps) mcp.ToolHandlerFor[searchInput, SearchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, SearchOutput, error) {
		params, mode, err := searchParams(input)
		if err != nil {
			return nil, SearchOutput{}, err
		}

		var searcher SearchAPI = deps.Archive
		if mode == "semantic" {
			searcher = deps.Semantic
		}
		if searcher == nil || deps.Archive == nil {
			return nil, SearchOutput{}, fmt.Errorf("%s search is unavailable", mode)
		}

		page, err := searcher.SearchMessages(ctx, params)
		if err != nil {
			return nil, SearchOutput{}, searchError(deps.Logger, mode, err)
		}
		if len(page.Results) == 0 && params.ChannelID != "" {
			// An empty page must not read as "never discussed" in a channel
			// that does not exist.
			_, found, err := deps.Archive.GetArchiveChannel(ctx, params.ChannelID)
			if err != nil {
				deps.Logger.Error("look up search channel", "channel_id", params.ChannelID, "error", err)
				return nil, SearchOutput{}, errors.New("search failed")
			}
			if !found {
				return nil, SearchOutput{}, errors.New("unknown channel_id: no archived channel has that ID; list_channels returns the archived channels")
			}
		}

		ids := make([]string, len(page.Results))
		for i, result := range page.Results {
			ids[i] = result.MessageID
		}
		messages, err := deps.Archive.GetArchiveMessages(ctx, ids)
		if err != nil {
			deps.Logger.Error("load search results", "mode", mode, "error", err)
			return nil, SearchOutput{}, errors.New("search failed")
		}
		byID := make(map[string]archive.ArchiveMessage, len(messages))
		for _, message := range messages {
			byID[message.ID] = message
		}

		output := SearchOutput{
			Results: make([]SearchResult, 0, len(page.Results)),
			HasMore: page.HasMore,
		}
		if page.HasMore {
			// Counts hits dropped below, so the next page repeats nothing.
			output.NextOffset = params.Offset + len(page.Results)
		}
		for _, result := range page.Results {
			message, ok := byID[result.MessageID]
			if !ok {
				continue // deleted since the search ran
			}
			out := SearchResult{
				Message:       messageView(message, searchContentLimit),
				ChannelID:     result.ChannelID,
				ChannelName:   result.ChannelName,
				CommunityName: result.CommunityName,
				HasAttachment: result.HasAttachment,
			}
			if mode == "fts" {
				out.Excerpt = result.Excerpt
			}
			if result.Distance != nil {
				distance := math.Round(*result.Distance*1e4) / 1e4
				out.Distance = &distance
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
	case errors.Is(err, archive.ErrQueryNotSearchable):
		return errors.New("query has no searchable words: keyword search ignores emoji and punctuation; search for words from the message, or use mode semantic")
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
