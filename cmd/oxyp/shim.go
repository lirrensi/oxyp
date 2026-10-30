package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// runShim bridges MCP stdio clients to the oxyp HTTP daemon.
// Speaks minimal JSON-RPC/MCP: initialize, tools/list, tools/call with the
// 5 meta-tools. Autostarts the daemon like the CLI does.

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      any     `json:"id"`
	Result  any     `json:"result,omitempty"`
	Error   *rpcErr `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var metaTools = []map[string]any{
	{
		"name":        "list_servers",
		"description": "List downstream MCP servers. Optional filter matches name/description.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"filter": map[string]any{"type": "string"},
		}},
	},
	{
		"name":        "list_commands",
		"description": "Thin tool list (name+description) for a server.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"server"}, "properties": map[string]any{
			"server": map[string]any{"type": "string"},
		}},
	},
	{
		"name":        "describe_commands",
		"description": "Full schemas for named tools on a server.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"server", "command_names"}, "properties": map[string]any{
			"server":        map[string]any{"type": "string"},
			"command_names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}},
	},
	{
		"name":        "invoke_command",
		"description": "Call a downstream tool. Args nest under parameters.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"server", "command_name"}, "properties": map[string]any{
			"server":       map[string]any{"type": "string"},
			"command_name": map[string]any{"type": "string"},
			"parameters":   map[string]any{"type": "object"},
		}},
	},
	{
		"name":        "search_tools",
		"description": "Ranked tool search across servers (words match name/server/description). Never spawns; searches cached metadata only.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{
			"query":  map[string]any{"type": "string"},
			"server": map[string]any{"type": "string"},
			"regex":  map[string]any{"type": "boolean"},
			"limit":  map[string]any{"type": "number"},
			"offset": map[string]any{"type": "number"},
		}},
	},
}

func runShim() error {
	e, err := ensureDaemon()
	if err != nil {
		return err
	}
	httpc := &http.Client{Timeout: timeout}
	get := func(path string) (any, error) {
		req, err := http.NewRequest("GET", e.URL+path, nil)
		if err != nil {
			return nil, err
		}
		if e.Token != "" {
			req.Header.Set("Authorization", "Bearer "+e.Token)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("daemon %d: %s", resp.StatusCode, b)
		}
		var v any
		return v, json.Unmarshal(b, &v)
	}
	post := func(path string, body any) (any, error) {
		b, _ := json.Marshal(body)
		req, err := http.NewRequest("POST", e.URL+path, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		if e.Token != "" {
			req.Header.Set("Authorization", "Bearer "+e.Token)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		var v any
		if err := json.Unmarshal(rb, &v); err != nil {
			return nil, fmt.Errorf("daemon %d: %s", resp.StatusCode, rb)
		}
		if resp.StatusCode >= 400 {
			if m, ok := v.(map[string]any); ok {
				// Prefer the daemon's actionable login hint when present.
				if ar, ok := m["auth_required"].(string); ok && ar != "" {
					return nil, fmt.Errorf("%v (%s)", m["error"], ar)
				}
				return nil, fmt.Errorf("%v", m["error"])
			}
			return nil, fmt.Errorf("daemon %d", resp.StatusCode)
		}
		return v, nil
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	respond := func(id any, result any, rerr *rpcErr) {
		b, _ := json.Marshal(rpcResp{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
		out.Write(b)
		out.WriteString("\n")
		out.Flush()
	}
	fail := func(id any, msg string) {
		respond(id, nil, &rpcErr{Code: -32000, Message: msg})
	}

	sc := bufio.NewScanner(os.Stdin)
	// 64 MiB lines: clients may send large base64 arguments (media in,
	// not just media out). Output side is a bufio.Writer, uncapped.
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			continue // notifications/parse errors: ignore
		}
		switch req.Method {
		case "initialize":
			respond(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "oxyp", "version": "0.1.0"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}, nil)
		case "notifications/initialized", "notifications/cancelled":
			// no response
		case "tools/list":
			respond(req.ID, map[string]any{"tools": metaTools}, nil)
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				fail(req.ID, "bad params")
				continue
			}
			native, err := dispatchNative(get, post, p.Name, p.Arguments)
			if err != nil {
				fail(req.ID, err.Error())
				continue
			}
			respond(req.ID, native, nil)
		case "ping":
			respond(req.ID, map[string]any{}, nil)
		default:
			fail(req.ID, "method not found: "+req.Method)
		}
	}
	return sc.Err()
}

// textResult wraps our own JSON (list/describe) in a single text block.
func textResult(v any) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
		"isError": false,
	}
}

// dispatchNative routes a meta-tool call. Discovery tools return our JSON as
// text; invoke_command forwards the downstream content blocks VERBATIM —
// image/audio/resource blocks ride natively so clients can render them or
// pass them to the model. No decode, no re-encode, no text wrap.
func dispatchNative(
	get func(string) (any, error),
	post func(string, any) (any, error),
	name string, args map[string]any,
) (map[string]any, error) {
	str := func(k string) string {
		if s, ok := args[k].(string); ok {
			return s
		}
		return ""
	}
	switch name {
	case "list_servers":
		path := "/servers"
		if f := str("filter"); f != "" {
			path += "?filter=" + url.QueryEscape(f)
		}
		v, err := get(path)
		if err != nil {
			return nil, err
		}
		return textResult(v), nil
	case "search_tools":
		q := str("query")
		if q == "" {
			return nil, fmt.Errorf("query required")
		}
		vals := url.Values{"q": {q}}
		if srv := str("server"); srv != "" {
			vals.Set("server", srv)
		}
		if b, ok := args["regex"].(bool); ok && b {
			vals.Set("regex", "1")
		}
		if n, ok := args["limit"].(float64); ok && n > 0 {
			vals.Set("limit", strconv.Itoa(int(n)))
		}
		if n, ok := args["offset"].(float64); ok && n > 0 {
			vals.Set("offset", strconv.Itoa(int(n)))
		}
		v, err := get("/tools?" + vals.Encode())
		if err != nil {
			return nil, err
		}
		return textResult(v), nil
	case "list_commands":
		srv := str("server")
		if srv == "" {
			return nil, fmt.Errorf("server required")
		}
		v, err := get("/servers/" + url.PathEscape(srv) + "/tools")
		if err != nil {
			return nil, err
		}
		return textResult(v), nil
	case "describe_commands":
		srv := str("server")
		if srv == "" {
			return nil, fmt.Errorf("server required")
		}
		var out []any
		if names, ok := args["command_names"].([]any); ok {
			for _, n := range names {
				ns, _ := n.(string)
				if ns == "" {
					continue
				}
				v, err := get("/servers/" + url.PathEscape(srv) + "/tools/" + url.PathEscape(ns))
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
		}
		return textResult(map[string]any{"schemas": out}), nil
	case "invoke_command":
		srv, cmd := str("server"), str("command_name")
		if srv == "" || cmd == "" {
			return nil, fmt.Errorf("server and command_name required")
		}
		params, _ := args["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		v, err := post("/servers/"+url.PathEscape(srv)+"/tools/"+url.PathEscape(cmd), map[string]any{"parameters": params})
		if err != nil {
			return nil, err
		}
		// Daemon returns {content, isError, structuredContent?, _meta?} —
		// forward as the tools/call result directly.
		if m, ok := v.(map[string]any); ok {
			if _, has := m["content"]; has {
				return m, nil
			}
		}
		return textResult(v), nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
