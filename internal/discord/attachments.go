package discord

import (
	"context"
	"fmt"
)

// RefreshURLsBatchLimit is the most URLs Discord accepts in one refresh
// request.
const RefreshURLsBatchLimit = 50

type refreshURLsRequest struct {
	AttachmentURLs []string `json:"attachment_urls"`
}

type refreshedURL struct {
	Original  string `json:"original"`
	Refreshed string `json:"refreshed"`
}

type refreshURLsResponse struct {
	RefreshedURLs []refreshedURL `json:"refreshed_urls"`
}

// RefreshAttachmentURLs exchanges attachment URLs for working ones,
// keyed by the URL passed in.
//
// Discord signs CDN URLs with a short-lived token, so a stored URL stops
// working after roughly a day. Only the URL's path matters here: a URL
// whose signature has expired — or is absent altogether — is still
// refreshable, which is what makes an old archive recoverable.
//
// A refreshed URL does not necessarily get a full fresh lifetime, so
// callers must download immediately rather than refreshing ahead of time.
func (c *Client) RefreshAttachmentURLs(ctx context.Context, urls []string) (map[string]string, error) {
	if len(urls) == 0 {
		return map[string]string{}, nil
	}
	if len(urls) > RefreshURLsBatchLimit {
		return nil, fmt.Errorf("discord: refresh at most %d URLs per request, got %d",
			RefreshURLsBatchLimit, len(urls))
	}

	var resp refreshURLsResponse
	if err := c.post(ctx, "/attachments/refresh-urls", refreshURLsRequest{AttachmentURLs: urls}, &resp); err != nil {
		return nil, err
	}

	out := make(map[string]string, len(resp.RefreshedURLs))
	for _, r := range resp.RefreshedURLs {
		if r.Refreshed != "" {
			out[r.Original] = r.Refreshed
		}
	}
	return out, nil
}
