package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/config"
)

// listModels handles GET /v1/models. It returns an OpenAI-style list of the
// gateway-facing model ids so clients (and humans) can discover the catalog.
// This endpoint is public (no gateway token) to match the OpenAI convention.
func listModels(c *gin.Context) {
	all := config.AllModels()
	out := make([]gin.H, 0, len(all))
	for _, m := range all {
		out = append(out, gin.H{
			"id":                   m.ID,
			"object":               "model",
			"created":              0,
			"owned_by":             "opencode-sw",
			"name":                 m.Name,
			"protocol":             string(m.Protocol),
			"context_length":       m.ContextLen,
			"context_len":          m.ContextLen,
			"architecture":         m.Architecture,
			"pricing":              m.Pricing,
			"supported_parameters": m.SupportedParameters,
			"description":          m.Description,
			"knowledge_cutoff":     m.KnowledgeCutoff,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   out,
	})
}
