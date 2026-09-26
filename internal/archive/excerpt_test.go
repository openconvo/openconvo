package archive

import "testing"

func TestMarkTrimmed(t *testing.T) {
	const message = "The first part. The middle part. The last part."
	cases := []struct {
		name, content, excerpt, want string
	}{
		{"whole message", message,
			"The first part. The <mark>middle</mark> part. The last part.",
			"The first part. The <mark>middle</mark> part. The last part."},
		{"cut on both sides", message,
			"The <mark>middle</mark> part.",
			"…The <mark>middle</mark> part.…"},
		{"cut after", message,
			"The first part. The <mark>middle</mark>",
			"The first part. The <mark>middle</mark>…"},
		{"cut before", message,
			"<mark>middle</mark> part. The last part.",
			"…<mark>middle</mark> part. The last part."},
		{"only whitespace left out", "\nThe first part.\n",
			"The <mark>first</mark> part.",
			"The <mark>first</mark> part."},
		{"delimiter text in the message", "Wrap &lt;mark&gt;this&lt;/mark&gt; in tags, then the middle part.",
			"in tags, then the <mark>middle</mark> part.",
			"…in tags, then the <mark>middle</mark> part."},
		{"empty message", "", "", ""},
		{"headline position cannot be found", "The first part. The last part.",
			"The <mark>missing</mark> part.", "…The <mark>missing</mark> part.…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := markTrimmed(tc.content, tc.excerpt); got != tc.want {
				t.Errorf("markTrimmed() = %q, want %q", got, tc.want)
			}
		})
	}
}
