package preservation

import (
	"strings"
	"testing"
	"time"
)

func TestRenderMarkdownMessageTreatsContentAsLiteralText(t *testing.T) {
	content := "# not a heading\n<script>alert('no')</script>\n```"
	digest := strings.Repeat("a", 64)
	message := markdownMessage{
		ID:              "0198c0de-0000-7000-8000-000000000001",
		Content:         &content,
		SourceCreatedAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		AuthorName:      "author_[name]",
		Attachments: []markdownAttachment{{
			Filename: "notes [final].txt", Size: 12, DownloadStatus: "stored", SHA256: &digest,
		}},
	}
	var body strings.Builder
	writer := markdownWriter{writer: &body}
	renderMarkdownMessage(&writer, "0198c0aa-0000-7000-8000-000000000001", message)
	if writer.err != nil {
		t.Fatal(writer.err)
	}
	markdown := body.String()
	for _, want := range []string{
		"author\\_\\[name\\]",
		"    # not a heading",
		"    <script>alert('no')</script>",
		"    ```",
		"[notes \\[final\\]\\.txt](../../blobs/sha256/aa/" + digest + ")",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("rendered Markdown is missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "\n<script>") {
		t.Fatalf("message HTML escaped its literal block:\n%s", markdown)
	}
}

func TestMarkdownIndexUsesStableChannelIDs(t *testing.T) {
	manifest := Manifest{
		GeneratedAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		Communities: []ManifestCommunity{{ID: "community-1", Name: "Guild [one]"}},
	}
	body := renderMarkdownIndex(manifest, []markdownChannel{{
		ID: "channel-1", CommunityID: "community-1", Name: "general", Kind: "text", MessageCount: 3,
	}})
	if !strings.Contains(body, "## Guild \\[one\\]") || !strings.Contains(body, "[\\#general](channels/channel-1.md)") {
		t.Fatalf("index does not use escaped labels and stable IDs:\n%s", body)
	}
}

func TestMarkdownInlineNeutralizesHostileNames(t *testing.T) {
	hostile := "Ada\n===\n# forged heading\n> forged quote\n```\nfence\r\nAT&T"
	inline := markdownInline(hostile)
	if strings.ContainsAny(inline, "\r\n") {
		t.Fatalf("inline text still spans lines: %q", inline)
	}
	if !strings.Contains(inline, "AT\\&T") {
		t.Errorf("ampersand is not escaped: %q", inline)
	}

	message := markdownMessage{
		ID:              "0198c0de-0000-7000-8000-000000000001",
		SourceCreatedAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		AuthorName:      hostile,
		Bookmarks:       []markdownBookmark{{Title: hostile, Collection: hostile, Tags: []string{hostile}}},
		Attachments: []markdownAttachment{{
			Filename: hostile, Size: 1, DownloadStatus: "failed",
		}},
	}
	var body strings.Builder
	writer := markdownWriter{writer: &body}
	renderMarkdownMessage(&writer, "0198c0aa-0000-7000-8000-000000000001", message)
	if writer.err != nil {
		t.Fatal(writer.err)
	}
	index := renderMarkdownIndex(Manifest{
		GeneratedAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		Communities: []ManifestCommunity{{ID: "community-1", Name: hostile}},
	}, []markdownChannel{{ID: "channel-1", CommunityID: "community-1", Name: hostile, Kind: "text"}})

	for _, rendered := range []string{body.String(), index} {
		for _, line := range strings.Split(rendered, "\n") {
			switch {
			case strings.HasPrefix(line, "# forged"), strings.HasPrefix(line, "> forged"),
				strings.HasPrefix(line, "```"), strings.HasPrefix(line, "==="):
				t.Fatalf("hostile name forged Markdown structure %q in:\n%s", line, rendered)
			}
		}
	}
}
