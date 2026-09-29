package openai

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
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"t%d \"}}]}\n\n", i)
	})
}
