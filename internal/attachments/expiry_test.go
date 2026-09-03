package attachments

import (
	"testing"
	"time"
)

func TestURLExpiry(t *testing.T) {
	// 0x6a85b265 is a real expiry value from a Discord CDN URL.
	when, ok := urlExpiry("https://cdn.discordapp.com/attachments/1/2/x.png?ex=6a85b265&is=1&hm=2")
	if !ok {
		t.Fatal("ok = false for a URL with an ex= parameter")
	}
	if want := time.Unix(0x6a85b265, 0); !when.Equal(want) {
		t.Errorf("expiry = %v, want %v", when, want)
	}

	for _, raw := range []string{
		"https://cdn.discordapp.com/attachments/1/2/x.png",
		"https://cdn.discordapp.com/attachments/1/2/x.png?ex=notahexnumber",
		"://not a url",
	} {
		if _, ok := urlExpiry(raw); ok {
			t.Errorf("urlExpiry(%q) ok = true, want false", raw)
		}
	}
}
