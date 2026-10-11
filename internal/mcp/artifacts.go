package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type artifactListIn struct {
	Session string `json:"session,omitempty" jsonschema:"session id/prefix/'latest' to list that session's artifacts; empty lists host artifacts (flash backups, exports) below the hwlab artifacts directory"`
}

func (t *tools) artifactList(_ context.Context, _ *sdk.CallToolRequest, in artifactListIn) (*sdk.CallToolResult, obj, error) {
	refs, err := t.app.ListArtifacts(in.Session)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"artifacts": refs, "count": len(refs), "root": t.app.ArtifactsRoot()}, nil
}

type artifactRefIn struct {
	Ref string `json:"ref" jsonschema:"artifact reference from artifact_list or a tool result: file/<path> or session/<id>/<artifact>"`
}

func (t *tools) artifactInfo(_ context.Context, _ *sdk.CallToolRequest, in artifactRefIn) (*sdk.CallToolResult, obj, error) {
	r, err := t.app.ArtifactInfo(in.Ref)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"artifact": r}, nil
}

type artifactReadIn struct {
	Ref      string `json:"ref"`
	Offset   int64  `json:"offset,omitempty"`
	Length   int    `json:"length,omitempty" jsonschema:"bytes to read (default 4096, max 65536); read large artifacts in windows"`
	Encoding string `json:"encoding,omitempty" jsonschema:"hex (default) or text"`
}

func (t *tools) artifactRead(_ context.Context, _ *sdk.CallToolRequest, in artifactReadIn) (*sdk.CallToolResult, obj, error) {
	c, err := t.app.ReadArtifact(in.Ref, in.Offset, in.Length, in.Encoding)
	if err != nil {
		return nil, nil, err
	}
	return nil, obj{"chunk": c}, nil
}

func (t *tools) capabilityRegistry(ctx context.Context, _ *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, obj, error) {
	return nil, obj{"registry": t.app.Registry(ctx)}, nil
}
