package mcp

// Small builders for the JSON Schemas tools/list advertises. The shapes follow what upstream's
// Python server generates from its signatures - `X | None = None` becomes an anyOf with null
// and a null default - so a client that cached one server's schemas is not surprised by the
// other's.

type schema = map[string]any

func object(props map[string]schema, required ...string) schema {
	s := schema{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	} else {
		s["required"] = []string{}
	}

	return s
}

func typed(t, desc string) schema {
	s := schema{"type": t}
	if desc != "" {
		s["description"] = desc
	}

	return s
}

func str(desc string) schema     { return typed("string", desc) }
func number(desc string) schema  { return typed("number", desc) }
func integer(desc string) schema { return typed("integer", desc) }
func boolean(desc string) schema { return typed("boolean", desc) }

func withDefault(s schema, v any) schema {
	s["default"] = v

	return s
}

func arrayOf(items schema, desc string) schema {
	s := schema{"type": "array", "items": items}
	if desc != "" {
		s["description"] = desc
	}

	return s
}

func stringMap(desc string) schema {
	return schema{"type": "object", "additionalProperties": schema{"type": "string"}, "description": desc}
}

// optional is `T | None = None`.
func optional(s schema) schema {
	desc, _ := s["description"].(string)
	delete(s, "description")

	out := schema{"anyOf": []schema{s, {"type": "null"}}, "default": nil}
	if desc != "" {
		out["description"] = desc
	}

	return out
}

func connectIfMissing() schema {
	return withDefault(boolean("Accepted for compatibility with upstream's server. sbx resolves any "+
		"sandbox_id on demand, so a sandbox from sandbox_list works without sandbox_connect first."), false)
}

func sandboxID() schema {
	return str("Sandbox identifier, as returned by sandbox_create, sandbox_connect or sandbox_list.")
}

func ptr[T any](v T) *T { return &v }
