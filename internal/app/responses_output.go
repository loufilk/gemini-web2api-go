package app

// responseOutputItems follows streaming declaration order, not tool parsing order.
// No tool completion is published until generation and identity validation succeed.
func responseOutputItems(messageID, text string, toolCalls []ToolCall, messageDeclared bool) []map[string]interface{} {
	output := make([]map[string]interface{}, 0, len(toolCalls)+1)
	if messageDeclared || text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]interface{}{
			"type": "message", "id": messageID, "role": "assistant", "status": "completed",
			"content": []map[string]interface{}{{"type": "output_text", "text": text, "annotations": []interface{}{}}},
		})
	}
	for _, tc := range toolCalls {
		output = append(output, map[string]interface{}{
			"type": "function_call", "id": tc.ID, "call_id": tc.ID,
			"name": tc.Function.Name, "arguments": tc.Function.Arguments, "status": "completed",
		})
	}
	return output
}
