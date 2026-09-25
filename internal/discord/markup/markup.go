// Package markup renders Discord's inline message markup, such as <@id>, for
// reading. The archive keeps the markup; only display copies are rewritten.
package markup

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
)

// Names looks up names by Discord ID. messageIDs are the messages the text
// came from, which can name people who never posted. *archive.Store
// satisfies it.
type Names interface {
	DisplayNames(ctx context.Context, source string, actorIDs, channelIDs, messageIDs []string) (actors, channels map[string]string, err error)
}

// Text is text to render and its message's ID, empty for a channel topic.
type Text struct {
	MessageID string
	Value     *string
}

// Capture groups: user ID (1), channel ID (2), emoji name (3), timestamp
// seconds (4) and style (5), command name (6). Role mentions are left out.
var markupPattern = regexp.MustCompile(`<@!?(\d{1,20})>` +
	`|<#(\d{1,20})>` +
	`|<a?:(\w{1,32}):\d{1,20}>` +
	`|<t:(-?\d{1,12})(?::([tTdDfFR]))?>` +
	`|</([-_\p{L}\p{N} ]{1,100}):\d{1,20}>`)

// codePattern matches code, which Discord shows literally.
var codePattern = regexp.MustCompile("(?s)```.*?```|``.*?``|`[^`]+`")

// Render rewrites the markup in each text in place, with one lookup for all
// names. Markup in code or escaped, and mentions without a name, stay as
// written; if the lookup fails, every text does. The values must be copies
// read for one response.
func Render(ctx context.Context, names Names, texts ...Text) error {
	type pending struct {
		value   *string
		matches [][]int
	}
	var work []pending
	var actorIDs, channelIDs, messageIDs []string
	seen := map[string]bool{}
	add := func(list *[]string, kind, id string) {
		if !seen[kind+id] {
			seen[kind+id] = true
			*list = append(*list, id)
		}
	}
	for _, text := range texts {
		if text.Value == nil {
			continue
		}
		matches := renderable(*text.Value)
		if len(matches) == 0 {
			continue
		}
		work = append(work, pending{value: text.Value, matches: matches})
		for _, match := range matches {
			if id := group(*text.Value, match, 1); id != "" {
				add(&actorIDs, "@", id)
				if text.MessageID != "" {
					add(&messageIDs, "m", text.MessageID)
				}
			}
			if id := group(*text.Value, match, 2); id != "" {
				add(&channelIDs, "#", id)
			}
		}
	}

	var actors, channels map[string]string
	if len(actorIDs) > 0 || len(channelIDs) > 0 {
		var err error
		actors, channels, err = names.DisplayNames(ctx, archive.SourceDiscord, actorIDs, channelIDs, messageIDs)
		if err != nil {
			return fmt.Errorf("look up mentioned names: %w", err)
		}
	}
	for _, item := range work {
		*item.value = replace(*item.value, item.matches, actors, channels)
	}
	return nil
}

// renderable returns the markup Discord renders: outside code, not escaped.
func renderable(text string) [][]int {
	matches := markupPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	code := codePattern.FindAllStringIndex(text, -1)
	kept := matches[:0]
	for _, match := range matches {
		if !escaped(text, match[0]) && !within(code, match[0]) {
			kept = append(kept, match)
		}
	}
	return kept
}

func replace(text string, matches [][]int, actors, channels map[string]string) string {
	var out strings.Builder
	last := 0
	for _, match := range matches {
		out.WriteString(text[last:match[0]])
		out.WriteString(rendering(text, match, actors, channels))
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

func rendering(text string, match []int, actors, channels map[string]string) string {
	switch {
	case group(text, match, 1) != "":
		if name := actors[group(text, match, 1)]; name != "" {
			return "@" + name
		}
	case group(text, match, 2) != "":
		if name := channels[group(text, match, 2)]; name != "" {
			return "#" + name
		}
	case group(text, match, 3) != "":
		return ":" + group(text, match, 3) + ":"
	case group(text, match, 4) != "":
		return timestamp(group(text, match, 4), group(text, match, 5))
	case group(text, match, 6) != "":
		return "/" + group(text, match, 6)
	}
	return text[match[0]:match[1]]
}

func group(text string, match []int, n int) string {
	if match[2*n] < 0 {
		return ""
	}
	return text[match[2*n]:match[2*n+1]]
}

func escaped(text string, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && text[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func within(spans [][]int, at int) bool {
	for _, span := range spans {
		if at >= span[0] && at < span[1] {
			return true
		}
	}
	return false
}

// timestamp renders in UTC, with the time: a UTC date alone would be a day
// off for much of the world.
func timestamp(seconds, style string) string {
	unix, _ := strconv.ParseInt(seconds, 10, 64) // at most 12 digits, per markupPattern
	at := time.Unix(unix, 0).UTC()
	switch style {
	case "t":
		return at.Format("15:04") + " UTC"
	case "T":
		return at.Format("15:04:05") + " UTC"
	default:
		return at.Format("2006-01-02 15:04") + " UTC"
	}
}
