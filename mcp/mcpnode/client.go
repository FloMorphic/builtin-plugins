package mcpnode

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// newMCPClient builds, starts and initializes an MCP client for the connection.
// The transport is chosen by conn.Transport; a bearer/auth token, when present,
// is sent as an Authorization header for the HTTP-family transports. The caller
// owns the returned client and must Close it.
func newMCPClient(ctx context.Context, conn McpConnection) (*mcpclient.Client, error) {
	url := strings.TrimSpace(conn.URL)
	if url == "" {
		return nil, fmt.Errorf("mcp connection url is required")
	}
	headers := map[string]string{}
	if tok := strings.TrimSpace(conn.Auth); tok != "" {
		// Accept either a bare token or a full "Scheme value" header. A bare token
		// is treated as a bearer token, the common MCP case.
		if strings.Contains(tok, " ") {
			headers["Authorization"] = tok
		} else {
			headers["Authorization"] = "Bearer " + tok
		}
	}

	var (
		cli *mcpclient.Client
		err error
	)
	switch strings.ToLower(strings.TrimSpace(conn.Transport)) {
	case "", "streamable-http", "http", "streamable":
		opts := []mcptransport.StreamableHTTPCOption{}
		if len(headers) > 0 {
			opts = append(opts, mcptransport.WithHTTPHeaders(headers))
		}
		cli, err = mcpclient.NewStreamableHttpClient(url, opts...)
	case "sse":
		opts := []mcptransport.ClientOption{}
		if len(headers) > 0 {
			opts = append(opts, mcptransport.WithHeaders(headers))
		}
		cli, err = mcpclient.NewSSEMCPClient(url, opts...)
	case "stdio":
		// For stdio the "url" is the command line: `cmd arg1 arg2 ...`.
		parts := strings.Fields(url)
		cli, err = mcpclient.NewStdioMCPClient(parts[0], nil, parts[1:]...)
	default:
		return nil, fmt.Errorf("unsupported mcp transport %q", conn.Transport)
	}
	if err != nil {
		return nil, err
	}

	if err := cli.Start(ctx); err != nil {
		return nil, err
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "inflow-mcp-node", Version: "v0.1.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, fmt.Errorf("mcp initialize failed: %w", err)
	}
	return cli, nil
}

// listMCPTools fetches and normalizes the server's tools into the frontend shape.
func listMCPTools(ctx context.Context, cli *mcpclient.Client) ([]McpTool, error) {
	res, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]McpTool, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, McpTool{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: toolSchemaMap(t.InputSchema),
		})
	}
	return out, nil
}

// toolSchemaMap flattens mcp's typed input schema into a plain JSON-schema map —
// the `{type, properties, required, ...}` object the frontend expects and that
// langchaingo forwards to the provider as the tool's parameters.
func toolSchemaMap(s mcp.ToolInputSchema) map[string]any {
	m := map[string]any{"type": s.Type}
	if s.Properties != nil {
		m["properties"] = s.Properties
	} else {
		m["properties"] = map[string]any{}
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.Defs != nil {
		m["$defs"] = s.Defs
	}
	if s.AdditionalProperties != nil {
		m["additionalProperties"] = s.AdditionalProperties
	}
	return m
}

// callResultText renders a tool result as a single string for the model / the
// node output: it concatenates every text content block. A tool that reported an
// error is prefixed so the model can react to the failure.
func callResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if txt := mcp.GetTextFromContent(c); txt != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(txt)
		}
	}
	out := b.String()
	if res.IsError {
		return "tool error: " + out
	}
	return out
}

// callMCPTool invokes one tool. The model streams arguments as a raw JSON string;
// we decode it into a map for the MCP request, tolerating an empty/absent body.
func callMCPTool(ctx context.Context, cli *mcpclient.Client, name, rawArgs string) (string, error) {
	args := map[string]any{}
	if s := strings.TrimSpace(rawArgs); s != "" {
		if err := sonic.Unmarshal([]byte(s), &args); err != nil {
			return "", fmt.Errorf("bad tool arguments for %s: %w", name, err)
		}
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		return "tool error: " + err.Error(), err
	}
	return callResultText(res), nil
}
