package api

import (
	"encoding/json"
	"log"
	"strings"
)

// requestHead is the minimal information extracted from a JSON request body.
// It does NOT know about models, providers, or routing — that is the job of
// the router package.
type requestHead struct {
	Body     []byte
	Model    string
	Stream   bool
	Parsed   bool
	HasModel bool
}

// inspectRequestBody parses a JSON request body just enough to find the
// top-level "model" and "stream" fields. Invalid JSON or missing model is
// logged and forwarded unchanged. No model mapping or routing happens here.
func inspectRequestBody(path string, body []byte) requestHead {
	head := requestHead{Body: body}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		log.Printf("warn: request body parse skipped for %s: not valid JSON: %v", path, err)
		return head
	}
	head.Parsed = true

	if stream, ok := m["stream"].(bool); ok {
		head.Stream = stream
	}

	model, ok := m["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		log.Printf("warn: no string model field in request for %s", path)
		return head
	}
	head.Model = model
	head.HasModel = true
	return head
}