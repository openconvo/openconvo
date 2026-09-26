package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openconvo/openconvo/internal/discord/markup"
)

const channelsInputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`

const channelsDescription = "List the archived channels that search_messages and get_message_context read: " +
	"each channel's ID for the channel_id filter, its name, kind, topic and parent, how many live messages it holds, " +
	"and when the latest was posted. A thread is a channel of its own, so filtering by a parent channel leaves out " +
	"its threads. A channel missing from this list is not archived."

type channelsInput struct{}

type ChannelsOutput struct {
	Channels []Channel `json:"channels"`
}

func channelsHandler(deps Deps) mcp.ToolHandlerFor[channelsInput, ChannelsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ channelsInput) (*mcp.CallToolResult, ChannelsOutput, error) {
		if deps.Archive == nil {
			return nil, ChannelsOutput{}, errors.New("channel list is unavailable")
		}
		channels, err := deps.Archive.ListArchiveChannels(ctx)
		if err != nil {
			deps.Logger.Error("list archive channels", "error", err)
			return nil, ChannelsOutput{}, errors.New("listing channels failed")
		}
		topics := make([]markup.Text, len(channels))
		for i := range channels {
			topics[i] = markup.Text{Value: &channels[i].Topic}
		}
		renderMarkup(ctx, deps, topics...)
		output := ChannelsOutput{Channels: make([]Channel, 0, len(channels))}
		for _, channel := range channels {
			// Categories hold no messages.
			if channel.Kind == "category" {
				continue
			}
			output.Channels = append(output.Channels, channelView(channel))
		}
		return nil, output, nil
	}
}
