package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openconvo/openconvo/internal/discord/markup"
)

// maxContextMessages bounds each side of a context window. The store is asked
// for one more, to tell whether the channel continues.
const maxContextMessages = 25

const contextInputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "message_id": {
      "type": "string",
      "description": "OpenConvo message UUID, from a search result or an earlier context."
    },
    "before": {
      "type": "integer",
      "minimum": 0,
      "maximum": 25,
      "default": 5,
      "description": "Earlier messages from the same channel to include."
    },
    "after": {
      "type": "integer",
      "minimum": 0,
      "maximum": 25,
      "default": 5,
      "description": "Later messages from the same channel to include."
    }
  },
  "required": ["message_id"]
}`

const contextDescription = "Read an archived message in its conversation: the message with up to before earlier " +
	"and after later messages from the same channel, oldest first, each with its full text, reply link, attachments " +
	"and reactions. Pass a message_id from search_messages, or the first or last message of an earlier context to " +
	"read further; more_before and more_after say whether the channel continues. A thread is a channel of its own. " +
	"Deleted messages are never returned."

type contextInput struct {
	MessageID string `json:"message_id"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
}

type ContextOutput struct {
	Channel         Channel   `json:"channel"`
	TargetMessageID string    `json:"target_message_id"`
	Messages        []Message `json:"messages"`
	MoreBefore      bool      `json:"more_before"`
	MoreAfter       bool      `json:"more_after"`
}

func contextHandler(deps Deps) mcp.ToolHandlerFor[contextInput, ContextOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input contextInput) (*mcp.CallToolResult, ContextOutput, error) {
		// The store's IDs are lower case.
		messageID := strings.ToLower(strings.TrimSpace(input.MessageID))
		if !uuidPattern.MatchString(messageID) {
			return nil, ContextOutput{}, errors.New("message_id must be a UUID")
		}
		if input.Before < 0 || input.Before > maxContextMessages || input.After < 0 || input.After > maxContextMessages {
			return nil, ContextOutput{}, errors.New("before and after must be between 0 and 25")
		}
		if deps.Archive == nil {
			return nil, ContextOutput{}, errors.New("message context is unavailable")
		}

		conversation, found, err := deps.Archive.GetMessageContext(ctx, messageID, input.Before+1, input.After+1)
		if err != nil {
			deps.Logger.Error("load message context", "message_id", messageID, "error", err)
			return nil, ContextOutput{}, errors.New("loading message context failed")
		}
		target := -1
		for i, message := range conversation.Messages {
			if message.ID == messageID {
				target = i
			}
		}
		if !found || target < 0 {
			return nil, ContextOutput{}, errors.New("message not found")
		}

		texts := make([]markup.Text, 0, len(conversation.Messages)+1)
		for _, message := range conversation.Messages {
			texts = append(texts, markup.Text{MessageID: message.ID, Value: message.Content})
		}
		texts = append(texts, markup.Text{Value: &conversation.Channel.Topic})
		renderMarkup(ctx, deps, texts...)

		earlier, later := conversation.Messages[:target], conversation.Messages[target+1:]
		output := ContextOutput{
			Channel:         channelView(conversation.Channel),
			TargetMessageID: messageID,
			MoreBefore:      len(earlier) > input.Before,
			MoreAfter:       len(later) > input.After,
		}
		if output.MoreBefore {
			earlier = earlier[len(earlier)-input.Before:]
		}
		if output.MoreAfter {
			later = later[:input.After]
		}
		output.Messages = make([]Message, 0, len(earlier)+1+len(later))
		for _, message := range earlier {
			output.Messages = append(output.Messages, messageView(message, 0))
		}
		output.Messages = append(output.Messages, messageView(conversation.Messages[target], 0))
		for _, message := range later {
			output.Messages = append(output.Messages, messageView(message, 0))
		}
		return nil, output, nil
	}
}
