package assist

import (
	"context"
	"encoding/json"
	"strings"
)

// MCP: the Model Context Protocol (JSON-RPC 2.0), the part agents use: initialize, tools/list, tools/call, ping.
// Homeport answers it on its root-only socket; `homeport mcp` carries it over stdin/stdout for opencode, Claude Code
// and other agents.

// ProtocolVersion is the MCP revision Homeport speaks (it answers older clients with theirs).
const ProtocolVersion = "2025-06-18"

var knownVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

// Instructions tell every agent how to work here (sent with initialize).
const Instructions = `You help an admin run this Homeport server (apps, users, domains on Ubuntu, behind Caddy, apps in Docker).
- Call read_memory first. If work is in progress, continue it (or confirm with the admin first if it's stale).
- Look tools run straight away. Change tools wait for an admin to approve them in Homeport: say what you're asking for and why before you call one, and if it's declined, ask what they'd prefer instead.
- Diagnose before you change: look at app_logs, app_details, system_health and check_domain first, and prefer the smallest change that fixes the problem.
- After each change, and whenever you learn something about an app's setup or quirks, write it down with remember (topic "server", "app: <id>" …), so the next agent knows what was done to what.
- If you stop before a task is finished, or are waiting on the admin, call set_handoff with what's done and what's next; clear it (empty text) when the task is complete.
- Secrets are hidden from you on purpose ("••••••"). Never try to read or reveal them.`

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// HandleMCP answers one JSON-RPC message (or a batch). It returns nil for notifications, which get no reply.
// client names who's asking, for the audit log and memory notes.
func (a *Assistant) HandleMCP(ctx context.Context, client string, body []byte) []byte {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var batch []json.RawMessage
		if json.Unmarshal(body, &batch) != nil {
			return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		}
		var out []json.RawMessage
		for _, m := range batch {
			if r := a.HandleMCP(ctx, client, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustJSON(out)
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
	}
	if len(req.ID) == 0 { // a notification (initialized, cancelled…): nothing to answer
		return nil
	}
	res := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := ProtocolVersion
		if knownVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		res.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "homeport", "title": "Homeport", "version": a.Version},
			"instructions":    Instructions,
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		var tools []map[string]any
		for _, t := range a.Tools() {
			props := t.Params
			if props == nil {
				props = map[string]any{}
			}
			required := t.Required
			if required == nil {
				required = []string{}
			}
			desc := t.Description
			switch t.Level {
			case Change:
				desc += " [Waits for an admin's approval.]"
			case Shell:
				desc += " [Shell: waits for an admin's approval.]"
			}
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": desc,
				"inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false},
				"annotations": map[string]any{"readOnlyHint": t.Level == Look, "destructiveHint": t.Level != Look, "openWorldHint": false},
			})
		}
		res.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments Args   `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			res.Error = &rpcError{-32602, "tools/call needs a tool name and arguments"}
			break
		}
		if p.Arguments == nil {
			p.Arguments = Args{}
		}
		r := a.Call(ctx, client, p.Name, p.Arguments)
		res.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": r.Text}}, "isError": r.IsError}
	default:
		res.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return mustJSON(res)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
