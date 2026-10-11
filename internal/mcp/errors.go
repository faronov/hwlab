package mcp

import (
	"context"
	"encoding/json"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/faronov/hwlab/internal/app"
)

// add registers a typed tool whose failures are returned as MCP tool results
// with isError=true and a machine-readable error object
// {code, message, retryable, hint}; clients never need to parse text.
func add[In any](s *sdk.Server, tool *sdk.Tool, h func(context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, obj, error)) {
	sdk.AddTool(s, tool, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, obj, error) {
		res, out, err := h(ctx, req, in)
		if err != nil {
			return failure(err, out)
		}
		return res, out, nil
	})
}

// failure builds the error result. extra (e.g. "partial") is merged in.
func failure(err error, extra obj) (*sdk.CallToolResult, obj, error) {
	info := app.Classify(err)
	out := obj{"ok": false, "error": info}
	for k, v := range extra {
		out[k] = v
	}
	b, _ := json.Marshal(info)
	res := &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: info.Code + ": " + info.Message + "\n" + string(b)}}}
	return res, out, nil
}
