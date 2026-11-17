package app

import (
	"fmt"
	"strings"
)

// normalizeResponsesInput converts the stateless Responses input to the shared
// message representation without dropping tool history or multimodal parts.
// Server-side response storage is not implemented; never pretend a continuation
// succeeded with the preceding conversation missing.
func normalizeResponsesInput(req map[string]interface{}) ([]map[string]interface{}, error) {
	if prev, ok := req["previous_response_id"]; ok && prev != nil {
		if s, valid := prev.(string); !valid || s != "" {
			return nil, fmt.Errorf("previous_response_id is not supported; send the complete conversation in input")
		}
	}
	if conversation, ok := req["conversation"]; ok && conversation != nil {
		if value, valid := conversation.(string); !valid || value != "" {
			return nil, fmt.Errorf("conversation is not supported; send the complete conversation in input")
		}
	}
	if store, ok := req["store"]; ok && store != nil {
		value, valid := store.(bool)
		if !valid || value {
			return nil, fmt.Errorf("response storage is not supported; use store:false")
		}
	}
	if err := validateResponsesToolChoice(req); err != nil {
		return nil, err
	}
	var messages []map[string]interface{}
	if instr, exists := req["instructions"]; exists && instr != nil {
		s, ok := instr.(string)
		if !ok {
			return nil, fmt.Errorf("instructions must be a string")
		}
		if s != "" {
			messages = append(messages, map[string]interface{}{"role": "system", "content": s})
		}
	}
	calls := map[string]string{}
	appendCall := func(item map[string]interface{}) (map[string]interface{}, error) {
		id, name := getStr(item, "call_id"), getStr(item, "name")
		args, ok := item["arguments"].(string)
		if id == "" || name == "" || !ok {
			return nil, fmt.Errorf("function_call requires call_id, name and string arguments")
		}
		if _, exists := calls[id]; exists {
			return nil, fmt.Errorf("duplicate function_call call_id")
		}
		calls[id] = name
		return map[string]interface{}{"id": id, "type": "function",
			"function": map[string]interface{}{"name": name, "arguments": args}}, nil
	}
	switch input := req["input"].(type) {
	case string:
		messages = append(messages, map[string]interface{}{"role": "user", "content": input})
	case []interface{}:
		for i, raw := range input {
			if s, ok := raw.(string); ok {
				messages = append(messages, map[string]interface{}{"role": "user", "content": s})
				continue
			}
			item, ok := raw.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("input[%d] must be a message or supported item", i)
			}
			switch typ := getStr(item, "type"); typ {
			case "function_call":
				tc, err := appendCall(item)
				if err != nil {
					return nil, err
				}
				messages = append(messages, map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{tc}})
			case "function_call_output":
				id := getStr(item, "call_id")
				if id == "" {
					return nil, fmt.Errorf("function_call_output requires call_id")
				}
				content, _, err := normalizeResponsesContent(item["output"], false)
				if err != nil {
					return nil, fmt.Errorf("function_call_output: %w", err)
				}
				name := calls[id]
				if name == "" {
					name = firstNonEmpty(getStr(item, "name"), id)
				}
				messages = append(messages, map[string]interface{}{"role": "tool", "tool_call_id": id, "name": name, "content": content})
			case "", "message":
				role := getStr(item, "role")
				if role == "" {
					role = "user"
				}
				switch role {
				case "user", "assistant", "system":
				case "developer":
					role = "system"
				default:
					return nil, fmt.Errorf("input[%d] has unsupported role", i)
				}
				content, nested, err := normalizeResponsesContent(item["content"], role == "assistant")
				if err != nil {
					return nil, fmt.Errorf("input[%d]: %w", i, err)
				}
				msg := map[string]interface{}{"role": role, "content": content}
				var tcs []interface{}
				for _, tc := range nested {
					call, err := appendCall(tc)
					if err != nil {
						return nil, err
					}
					tcs = append(tcs, call)
				}
				if len(tcs) > 0 {
					msg["tool_calls"] = tcs
				}
				messages = append(messages, msg)
			default:
				return nil, fmt.Errorf("input[%d] item type %q is not supported", i, typ)
			}
		}
	case nil:
	default:
		return nil, fmt.Errorf("input must be a string or array")
	}
	return messages, nil
}

func normalizeResponsesContent(raw interface{}, allowCalls bool) (interface{}, []map[string]interface{}, error) {
	if s, ok := raw.(string); ok {
		return s, nil, nil
	}
	parts, ok := raw.([]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("content must be a string or array")
	}
	out := make([]interface{}, 0, len(parts))
	var calls []map[string]interface{}
	for _, part := range parts {
		m, ok := part.(map[string]interface{})
		if !ok {
			return nil, nil, fmt.Errorf("content parts must be objects")
		}
		switch typ := getStr(m, "type"); typ {
		case "input_text", "output_text", "text":
			text, ok := m["text"].(string)
			if !ok {
				return nil, nil, fmt.Errorf("text content requires string text")
			}
			out = append(out, map[string]interface{}{"type": "text", "text": text})
		case "input_image", "image_url", "input_video", "video_url":
			key := "image_url"
			if typ == "input_video" || typ == "video_url" {
				key = "video_url"
			}
			source := firstNonEmpty(getStr(m, key), getStr(m, "url"), getStr(m, "data"))
			if nested, ok := m[key].(map[string]interface{}); ok && (typ == "image_url" || typ == "video_url") {
				source = getStr(nested, "url")
			}
			if strings.TrimSpace(source) == "" {
				return nil, nil, fmt.Errorf("%s requires a URL or data URL; file_id references are not supported", typ)
			}
			// Keep the original image/video structure until collectImages consumes
			// it. Converting to text here silently discards the attachment.
			out = append(out, m)
		case "function_call":
			if !allowCalls {
				return nil, nil, fmt.Errorf("function_call content is only valid in assistant history")
			}
			calls = append(calls, m)
		case "refusal":
			if s := getStr(m, "refusal"); s != "" {
				out = append(out, map[string]interface{}{"type": "text", "text": s})
			}
		default:
			return nil, nil, fmt.Errorf("content type %q is not supported", strings.TrimSpace(typ))
		}
	}
	return out, calls, nil
}

// validateResponsesToolChoice fails explicitly rather than silently treating an
// unsupported tool selector as auto.
func validateResponsesToolChoice(req map[string]interface{}) error {
	raw := req["tool_choice"]
	if raw == nil {
		return nil
	}
	switch choice := raw.(type) {
	case string:
		if choice == "auto" || choice == "none" || choice == "required" {
			return nil
		}
	case map[string]interface{}:
		if getStr(choice, "type") == "function" {
			_, name := parseToolChoice(choice)
			if name != "" {
				tools, _ := req["tools"].([]interface{})
				for _, item := range tools {
					tool, _ := item.(map[string]interface{})
					if getStr(tool, "type") != "function" {
						continue
					}
					fn, _ := tool["function"].(map[string]interface{})
					if firstNonEmpty(getStr(fn, "name"), getStr(tool, "name")) == name {
						return nil
					}
				}
				return fmt.Errorf("tool_choice names a function not declared in tools")
			}
		}
	}
	return fmt.Errorf("tool_choice is not supported; use auto, none, required or a declared function")
}
