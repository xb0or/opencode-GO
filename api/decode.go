package api

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/xb0or/opencode-GO/config"
)

type requestHead struct {
	Body     []byte
	Model    string
	Stream   bool
	Parsed   bool
	HasModel bool
	Mapped   bool
}

// inspectAndMapRequestBody parses a JSON request body just enough to find
// top-level "model" and "stream". If a configured model mapping matches, it
// rewrites the JSON body and returns the mapped model. Invalid JSON or missing
// model is logged and forwarded unchanged.
func inspectAndMapRequestBody(path string, body []byte) requestHead {
	head := requestHead{Body: body}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		log.Printf("warn: model mapping skipped for %s: request body is not valid JSON: %v", path, err)
		return head
	}
	head.Parsed = true

	if stream, ok := m["stream"].(bool); ok {
		head.Stream = stream
	}

	model, ok := m["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		log.Printf("warn: model mapping skipped for %s: request JSON has no string model field", path)
		return head
	}
	head.Model = model
	head.HasModel = true

	mapped, ok := config.LookupModelMapping(model)
	if !ok {
		return head
	}
	m["model"] = mapped
	out, err := json.Marshal(m)
	if err != nil {
		log.Printf("warn: model mapping %q -> %q skipped for %s: remarshal failed: %v", model, mapped, path, err)
		return head
	}
	head.Body = out
	head.Model = mapped
	head.Mapped = true
	log.Printf("model mapping applied for %s: %q -> %q", path, model, mapped)
	return head
}

func passthroughRoute(model string, inbound config.Protocol) config.ModelRoute {
	id := strings.TrimSpace(model)
	if id == "" {
		id = "passthrough"
	}
	return config.ModelRoute{
		ID:        id,
		Name:      id,
		Upstream:  config.UpstreamGo,
		Protocol:  inbound,
		RealModel: model,
		Group:     "go",
	}
}

// rewriteModel returns body with the top-level "model" field replaced. It
// re-marshals compact JSON; on any failure the original body is returned with
// ok=false so the caller keeps the original (model name may be a prefix match
// upstream, but that is acceptable degradation).
func rewriteModel(body []byte, realModel string) ([]byte, bool) {
	if realModel == "" {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	m["model"] = realModel
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// enableStreamUsage asks upstream protocols that support it to include final
// usage accounting in SSE streams so admin usage logs can record token counts.
func enableStreamUsage(body []byte, proto config.Protocol, stream bool) ([]byte, bool) {
	if !stream {
		return body, false
	}
	if proto != config.ProtocolChat {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	opts := objectField(m, "stream_options")
	if opts == nil {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	m["stream_options"] = opts
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}