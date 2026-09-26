package http

import (
	"context"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord/markup"
)

// renderMarkup renders Discord markup for the UI. If names cannot be looked
// up, the response goes out with the markup as written.
func renderMarkup(ctx context.Context, deps Deps, texts ...markup.Text) {
	if err := markup.Render(ctx, deps.Archive, texts...); err != nil {
		deps.Logger.Warn("render message markup", "error", err)
	}
}

func messageTexts(messages []archive.ArchiveMessage) []markup.Text {
	texts := make([]markup.Text, 0, 2*len(messages))
	for i := range messages {
		texts = append(texts, markup.Text{MessageID: messages[i].ID, Value: messages[i].Content})
		if reply := messages[i].ReplyTo; reply != nil {
			texts = append(texts, markup.Text{MessageID: reply.ID, Value: reply.Content})
		}
	}
	return texts
}

func topicText(channel *archive.ArchiveChannel) markup.Text {
	return markup.Text{Value: &channel.Topic}
}
