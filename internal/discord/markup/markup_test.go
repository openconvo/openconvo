package markup

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

type fakeNames struct {
	actors, channels map[string]string
	err              error
	calls            [][3][]string // sorted actor, channel and message IDs
	sources          []string
}

func (f *fakeNames) DisplayNames(_ context.Context, source string, actorIDs, channelIDs, messageIDs []string) (map[string]string, map[string]string, error) {
	f.calls = append(f.calls, [3][]string{
		slices.Sorted(slices.Values(actorIDs)),
		slices.Sorted(slices.Values(channelIDs)),
		slices.Sorted(slices.Values(messageIDs)),
	})
	f.sources = append(f.sources, source)
	return f.actors, f.channels, f.err
}

func newFakeNames() *fakeNames {
	return &fakeNames{
		actors: map[string]string{
			"111111111111111111": "Alice", "222222222222222222": "Bob", "888888888888888888": "",
		},
		channels: map[string]string{"333333333333333333": "help"},
	}
}

func TestRenderReplacesMarkupForReading(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"user mention", "Thanks <@111111111111111111>!", "Thanks @Alice!"},
		{"legacy nickname mention", "Thanks <@!222222222222222222>", "Thanks @Bob"},
		{"channel mention", "See <#333333333333333333>", "See #help"},
		{"custom emoji", "Nice <:clap_hands:444444444444444444>", "Nice :clap_hands:"},
		{"animated emoji", "<a:party:555555555555555555>", ":party:"},
		{"timestamp", "Starts <t:1756713600>", "Starts 2025-09-01 08:00 UTC"},
		{"relative timestamp", "<t:1756713600:R>", "2025-09-01 08:00 UTC"},
		{"date timestamp keeps the time", "<t:1756713600:D>", "2025-09-01 08:00 UTC"},
		{"time timestamp", "<t:1756713600:t>", "08:00 UTC"},
		{"long time timestamp", "<t:1756713600:T>", "08:00:00 UTC"},
		{"command", "Run </archive export:666666666666666666>", "Run /archive export"},
		{"unknown user", "Thanks <@999999999999999999>", "Thanks <@999999999999999999>"},
		{"unknown channel", "See <#999999999999999999>", "See <#999999999999999999>"},
		{"empty name", "Thanks <@888888888888888888>", "Thanks <@888888888888888888>"},
		{"role mention", "Ping <@&777777777777777777>", "Ping <@&777777777777777777>"},
		{"plain text", "I <3 this and 2 < 3", "I <3 this and 2 < 3"},
		{"unknown timestamp style", "<t:1756713600:x>", "<t:1756713600:x>"},
		{"inline code", "Type `<t:1756713600:R>` for a date", "Type `<t:1756713600:R>` for a date"},
		{"double backtick code", "``<@111111111111111111>``", "``<@111111111111111111>``"},
		{"code block", "```\n<@111111111111111111> <#333333333333333333>\n```\nThanks <@111111111111111111>",
			"```\n<@111111111111111111> <#333333333333333333>\n```\nThanks @Alice"},
		{"escaped", `Type \<:clap:444444444444444444> for the ID`, `Type \<:clap:444444444444444444> for the ID`},
		{"escaped backslash", `\\<:clap:444444444444444444>`, `\\:clap:`},
		{"unclosed backtick", "One ` only, <@111111111111111111>", "One ` only, @Alice"},
		{"highlighted ID", "<@<mark>111111111111111111</mark>>", "<@<mark>111111111111111111</mark>>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.in
			if err := Render(context.Background(), newFakeNames(), Text{Value: &text}); err != nil {
				t.Fatal(err)
			}
			if text != tc.want {
				t.Errorf("Render(%q) = %q, want %q", tc.in, text, tc.want)
			}
		})
	}
}

func TestRenderLooksUpAllNamesOnce(t *testing.T) {
	names := newFakeNames()
	first := "<@111111111111111111> and <@!222222222222222222>"
	second := "<@111111111111111111> in <#333333333333333333>"
	topic := "See <#333333333333333333>"
	if err := Render(context.Background(), names,
		Text{MessageID: "message-1", Value: &first},
		Text{MessageID: "message-2"},
		Text{MessageID: "message-3", Value: &second},
		Text{Value: &topic},
	); err != nil {
		t.Fatal(err)
	}
	want := [][3][]string{{
		{"111111111111111111", "222222222222222222"}, {"333333333333333333"}, {"message-1", "message-3"},
	}}
	if !reflect.DeepEqual(names.calls, want) || names.sources[0] != "discord" {
		t.Fatalf("lookups = %v from %v, want %v", names.calls, names.sources, want)
	}
	if first != "@Alice and @Bob" || second != "@Alice in #help" || topic != "See #help" {
		t.Fatalf("rendered %q, %q and %q", first, second, topic)
	}
}

func TestRenderSkipsTheLookupWithoutMentions(t *testing.T) {
	names := newFakeNames()
	text := "Nice <:clap:444444444444444444> <t:1756713600:d> `<@111111111111111111>`"
	if err := Render(context.Background(), names, Text{MessageID: "message-1", Value: &text}); err != nil {
		t.Fatal(err)
	}
	if len(names.calls) != 0 || text != "Nice :clap: 2025-09-01 08:00 UTC `<@111111111111111111>`" {
		t.Fatalf("lookups %v, text %q", names.calls, text)
	}
}

func TestRenderLeavesTextAloneWhenTheLookupFails(t *testing.T) {
	names := newFakeNames()
	names.err = errors.New("database unavailable")
	text := "Thanks <@111111111111111111> <:clap:444444444444444444>"
	if err := Render(context.Background(), names, Text{Value: &text}); err == nil ||
		text != "Thanks <@111111111111111111> <:clap:444444444444444444>" {
		t.Fatalf("err %v, text %q", err, text)
	}
}
