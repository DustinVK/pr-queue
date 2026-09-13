package runner

import (
	"encoding/json"
	"fmt"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/localfs"
)

func OutputSchema(input findings.Input) ([]byte, error) {
	stringEnum := func(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
	base := map[string]any{
		"kind": stringEnum(), "severity": stringEnum("nit", "minor", "major", "critical"),
		"category": stringEnum("correctness", "security", "performance", "style", "test-coverage", "design"),
		"title":    map[string]any{"type": "string"}, "body": map[string]any{"type": "string"},
	}
	variant := func(kind string, anchors []string, rationale bool) map[string]any {
		properties := make(map[string]any, len(base)+len(anchors)+1)
		for k, v := range base {
			properties[k] = v
		}
		properties["kind"] = stringEnum(kind)
		for _, name := range anchors {
			switch name {
			case "path", "side", "start_side":
				properties[name] = map[string]any{"type": "string"}
			default:
				properties[name] = map[string]any{"type": "integer"}
			}
		}
		if _, ok := properties["side"]; ok {
			properties["side"] = stringEnum("LEFT", "RIGHT")
		}
		if _, ok := properties["start_side"]; ok {
			properties["start_side"] = stringEnum("LEFT", "RIGHT")
		}
		required := []string{"kind", "severity", "category", "title", "body"}
		required = append(required, anchors...)
		if rationale {
			properties["rationale"] = map[string]any{"type": "string"}
			required = append(required, "rationale")
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	variants := []any{}
	for _, shape := range []struct {
		kind    string
		anchors []string
	}{{"general", nil}, {"inline", []string{"path", "side", "line"}}, {"inline", []string{"path", "side", "start_line", "start_side", "line"}}} {
		variants = append(variants, variant(shape.kind, shape.anchors, false), variant(shape.kind, shape.anchors, true))
	}
	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "integer", "enum": []int{1}},
			"repo":           stringEnum(input.Repo), "pr": map[string]any{"type": "integer", "enum": []int{input.PR}}, "head_sha": stringEnum(input.HeadSHA),
			"summary": map[string]any{"type": "string"}, "verdict": stringEnum("comment", "approve", "request_changes"),
			"findings": map[string]any{"type": "array", "maxItems": findings.MaxFindings, "items": map[string]any{"anyOf": variants}},
		},
		"required": []string{"schema_version", "repo", "pr", "head_sha", "summary", "verdict", "findings"},
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode findings output schema: %w", err)
	}
	return append(data, '\n'), nil
}

func WriteOutputSchema(path string, input findings.Input) error {
	data, err := OutputSchema(input)
	if err != nil {
		return err
	}
	created, err := localfs.WriteNew(path, data)
	if err != nil {
		return fmt.Errorf("write findings output schema: %w", err)
	}
	if !created {
		return fmt.Errorf("findings output schema already exists: %s", path)
	}
	return nil
}
