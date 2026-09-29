package anthropic

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/modeltest"
)

func TestStream_ReleasesAbandonedResponse(t *testing.T) {
	modeltest.Run(t, func(url string, c *http.Client) agent.Model {
		return New("k", WithBaseURL(url), WithHTTPClient(c))
	}, func(i int) string {
		return fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"t%d \"}}\n\n", i)
	})
}
