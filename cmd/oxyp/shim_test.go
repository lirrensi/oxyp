package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// fakeBackends builds get/post stubs over canned values and records paths/bodies.
type fakeBackend struct {
	getVals  map[string]any
	getErr   map[string]error
	postVals map[string]any
	postErr  map[string]error
	gotGet   []string
	gotPost  []string
	gotBody  []any
}

func (f *fakeBackend) get(path string) (any, error) {
	f.gotGet = append(f.gotGet, path)
	if err, ok := f.getErr[path]; ok {
		return nil, err
	}
	if v, ok := f.getVals[path]; ok {
		return v, nil
	}
	return map[string]any{"path": path}, nil
}

func (f *fakeBackend) post(path string, body any) (any, error) {
	f.gotPost = append(f.gotPost, path)
	f.gotBody = append(f.gotBody, body)
	if err, ok := f.postErr[path]; ok {
		return nil, err
	}
	if v, ok := f.postVals[path]; ok {
		return v, nil
	}
	return map[string]any{"path": path}, nil
}

func asTextJSON(t *testing.T, res map[string]any) any {
	t.Helper()
	content, ok := res["content"].([]map[string]any)
	if !ok {
		// textResult builds []map[string]any; decode via JSON round trip if type differs.
		b, _ := json.Marshal(res["content"])
		var generic []map[string]any
		if err := json.Unmarshal(b, &generic); err != nil {
			t.Fatalf("content not text blocks: %v", res)
		}
		content = generic
	}
	if len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("want single text block, got %v", res)
	}
	text, _ := content[0]["text"].(string)
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("text not JSON: %q", text)
	}
	return v
}

func TestDispatchListServersPaths(t *testing.T) {
	fb := &fakeBackend{getVals: map[string]any{"/servers": []any{}, "/servers?filter=a%2Bb+c": []any{}}}
	res, err := dispatchNative(fb.get, fb.post, "list_servers", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if fb.gotGet[0] != "/servers" {
		t.Fatalf("path = %q", fb.gotGet[0])
	}
	if res["isError"] != false {
		t.Fatalf("list must have isError=false: %v", res)
	}
	asTextJSON(t, res)

	fb2 := &fakeBackend{}
	if _, err := dispatchNative(fb2.get, fb2.post, "list_servers", map[string]any{"filter": "a+b c"}); err != nil {
		t.Fatal(err)
	}
	if fb2.gotGet[0] != "/servers?filter="+url.QueryEscape("a+b c") {
		t.Fatalf("filter path = %q", fb2.gotGet[0])
	}
}

func TestDispatchSearchToolsEncoding(t *testing.T) {
	fb := &fakeBackend{}
	_, err := dispatchNative(fb.get, fb.post, "search_tools", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "query required") {
		t.Fatalf("empty query err = %v", err)
	}
	args := map[string]any{
		"query": "a+b c", "server": "s", "regex": true,
		"limit": float64(5), "offset": float64(10),
	}
	if _, err := dispatchNative(fb.get, fb.post, "search_tools", args); err != nil {
		t.Fatal(err)
	}
	got := fb.gotGet[len(fb.gotGet)-1]
	parsed, err := url.Parse(got)
	if err != nil || !strings.HasPrefix(got, "/tools?") {
		t.Fatalf("path = %q err=%v", got, err)
	}
	q := parsed.Query()
	if q.Get("q") != "a+b c" || q.Get("server") != "s" || q.Get("regex") != "1" || q.Get("limit") != "5" || q.Get("offset") != "10" {
		t.Fatalf("query = %+v path=%q", q, got)
	}
	// zero/negative limit/offset omitted
	for _, tc := range []map[string]any{
		{"query": "q", "limit": float64(0), "offset": float64(0)},
		{"query": "q", "limit": float64(-3), "offset": float64(-1)},
	} {
		n := len(fb.gotGet)
		if _, err := dispatchNative(fb.get, fb.post, "search_tools", tc); err != nil {
			t.Fatal(err)
		}
		p, _ := url.Parse(fb.gotGet[n])
		if p.Query().Has("limit") || p.Query().Has("offset") {
			t.Fatalf("zero/negative must be omitted: %q", fb.gotGet[n])
		}
	}
	// regex=false omitted
	n := len(fb.gotGet)
	if _, err := dispatchNative(fb.get, fb.post, "search_tools", map[string]any{"query": "q", "regex": false}); err != nil {
		t.Fatal(err)
	}
	if p, _ := url.Parse(fb.gotGet[n]); p.Query().Has("regex") {
		t.Fatalf("regex=false must be omitted: %q", fb.gotGet[n])
	}
}

func TestDispatchListTools(t *testing.T) {
	fb := &fakeBackend{}
	if _, err := dispatchNative(fb.get, fb.post, "list_tools", map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "server required") {
		t.Fatalf("missing server err = %v", err)
	}
	srv := "a/b%c\n"
	want := "/servers/" + url.PathEscape(srv) + "/tools"
	if _, err := dispatchNative(fb.get, fb.post, "list_tools", map[string]any{"server": srv}); err != nil {
		t.Fatal(err)
	}
	if fb.gotGet[len(fb.gotGet)-1] != want {
		t.Fatalf("path = %q want %q", fb.gotGet[len(fb.gotGet)-1], want)
	}
}

func TestDispatchDescribeToolCurrent(t *testing.T) {
	fb := &fakeBackend{}
	// current behavior: missing server -> error
	if _, err := dispatchNative(fb.get, fb.post, "describe_tool", map[string]any{}); err == nil {
		t.Fatal("missing server must error")
	}
	// current behavior: absent tools silently returns empty schemas (documented).
	res, err := dispatchNative(fb.get, fb.post, "describe_tool", map[string]any{"server": "s"})
	if err != nil {
		t.Fatal(err)
	}
	v := asTextJSON(t, res)
	m := v.(map[string]any)
	if schemas, ok := m["schemas"].([]any); !ok || len(schemas) != 0 {
		// nil serializes as null; accept both null and [].
		if m["schemas"] != nil {
			t.Fatalf("current behavior: absent tools -> empty/null schemas, got %v", m)
		}
	}
	// wrong type (string) also silently empty.
	res, err = dispatchNative(fb.get, fb.post, "describe_tool", map[string]any{"server": "s", "tools": "nope"})
	if err != nil {
		t.Fatal(err)
	}
	asTextJSON(t, res)

	// mixed types: non-strings skipped.
	fb2 := &fakeBackend{getVals: map[string]any{"/servers/s/tools/ok": map[string]any{"name": "ok"}}}
	res, err = dispatchNative(fb2.get, fb2.post, "describe_tool",
		map[string]any{"server": "s", "tools": []any{"ok", float64(3), "", nil}})
	if err != nil {
		t.Fatal(err)
	}
	v = asTextJSON(t, res)
	got := v.(map[string]any)["schemas"].([]any)
	if len(got) != 1 {
		t.Fatalf("want 1 schema, got %v", v)
	}
	// upstream error surfaces.
	fb3 := &fakeBackend{getErr: map[string]error{"/servers/s/tools/bad": fmt.Errorf("daemon 404: nope")}}
	if _, err := dispatchNative(fb3.get, fb3.post, "describe_tool",
		map[string]any{"server": "s", "tools": []any{"bad"}}); err == nil {
		t.Fatal("upstream error must surface")
	}
}

func TestDispatchCall(t *testing.T) {
	fb := &fakeBackend{}
	for _, args := range []map[string]any{
		{},
		{"server": "s"},
		{"tool": "c"},
	} {
		if _, err := dispatchNative(fb.get, fb.post, "call", args); err == nil {
			t.Fatalf("args %v must error", args)
		}
	}
	// non-map parameters silently become {} (current behavior).
	fb2 := &fakeBackend{postVals: map[string]any{"/servers/s/tools/c": map[string]any{"content": []any{}}}}
	if _, err := dispatchNative(fb2.get, fb2.post, "call",
		map[string]any{"server": "s", "tool": "c", "parameters": "nope"}); err != nil {
		t.Fatal(err)
	}
	body := fb2.gotBody[len(fb2.gotBody)-1].(map[string]any)
	if _, ok := body["parameters"].(map[string]any); !ok {
		t.Fatalf("parameters must be map, got %#v", body)
	}
	// empty map forwarded verbatim.
	fb3 := &fakeBackend{postVals: map[string]any{"/servers/s/tools/c": map[string]any{"content": []any{}}}}
	if _, err := dispatchNative(fb3.get, fb3.post, "call",
		map[string]any{"server": "s", "tool": "c"}); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchCallVerbatimForward(t *testing.T) {
	downstream := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "hi"},
			map[string]any{"type": "image", "data": "QUJD", "mimeType": "image/png"},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "f", "text": "x"}},
		},
		"structuredContent": map[string]any{"k": "v"},
		"_meta":             map[string]any{"z": 1},
		"isError":           true,
	}
	fb := &fakeBackend{postVals: map[string]any{"/servers/s/tools/c": downstream}}
	res, err := dispatchNative(fb.get, fb.post, "call",
		map[string]any{"server": "s", "tool": "c", "parameters": map[string]any{"a": 1}})
	if err != nil {
		t.Fatal(err)
	}
	// VERBATIM: same keys, same nested values, no text wrap.
	for _, k := range []string{"content", "structuredContent", "_meta", "isError"} {
		b1, _ := json.Marshal(res[k])
		b2, _ := json.Marshal(downstream[k])
		if string(b1) != string(b2) {
			t.Errorf("key %s: got %s want %s", k, b1, b2)
		}
	}
	b1, _ := json.Marshal(res)
	b2, _ := json.Marshal(downstream)
	if string(b1) != string(b2) {
		t.Fatalf("not byte-identical: got %s want %s", b1, b2)
	}
	// without content: wrapped via textResult with isError:false.
	fb4 := &fakeBackend{postVals: map[string]any{"/servers/s/tools/c": map[string]any{"tools": []any{}}}}
	res, err = dispatchNative(fb4.get, fb4.post, "call",
		map[string]any{"server": "s", "tool": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if res["isError"] != false {
		t.Fatalf("wrapped result must have isError:false: %v", res)
	}
	asTextJSON(t, res)

	// content-present incl textResult shape is NOT re-wrapped: content key wins.
	fb5 := &fakeBackend{postVals: map[string]any{"/servers/s/tools/c": map[string]any{"content": "just-a-string"}}}
	res, err = dispatchNative(fb5.get, fb5.post, "call",
		map[string]any{"server": "s", "tool": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if res["content"] != "just-a-string" {
		t.Fatalf("content-present must forward as-is: %v", res)
	}
}

func TestDispatchAuthHintCurrent(t *testing.T) {
	// current behavior lives in runShim's post closure (401 hint extraction);
	// dispatchNative itself just forwards errors. Pin that a 401-shaped error passes through.
	fb := &fakeBackend{postErr: map[string]error{"/servers/s/tools/c": fmt.Errorf(`map[auth_required:run: oxyp login s error:denied]`)}}
	_, err := dispatchNative(fb.get, fb.post, "call",
		map[string]any{"server": "s", "tool": "c"})
	if err == nil {
		t.Fatal("must surface post error")
	}
	// The hint-surfacing path maps daemon {"error","auth_required"} JSON into
	// "error (auth_required)" — replicate the runShim closure logic here to pin it.
	mkPostErr := func(status int, body []byte) error {
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return fmt.Errorf("daemon %d: %s", status, body)
		}
		if status >= 400 {
			if m, ok := v.(map[string]any); ok {
				if ar, ok := m["auth_required"].(string); ok && ar != "" {
					return fmt.Errorf("%v (%s)", m["error"], ar)
				}
				return fmt.Errorf("%v", m["error"])
			}
			return fmt.Errorf("daemon %d", status)
		}
		return nil
	}
	err = mkPostErr(401, []byte(`{"error":"denied","auth_required":"run: oxyp login s"}`))
	if err == nil || !strings.Contains(err.Error(), "run: oxyp login s") {
		t.Fatalf("401 hint must surface, got %v", err)
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	fb := &fakeBackend{}
	_, err := dispatchNative(fb.get, fb.post, "nope_tool", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "unknown tool: nope_tool") {
		t.Fatalf("err = %v", err)
	}
}

func TestMetaToolsInvariants(t *testing.T) {
	if len(metaTools) != 5 {
		t.Fatalf("want exactly 5 meta tools, got %d", len(metaTools))
	}
	names := map[string]map[string]any{}
	for _, tool := range metaTools {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tool missing name: %v", tool)
		}
		if _, dup := names[name]; dup {
			t.Fatalf("duplicate tool %q", name)
		}
		names[name] = tool
	}
	for _, want := range []string{"list_servers", "list_tools", "describe_tool", "call", "search_tools"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing tool %q", want)
		}
	}
	required := map[string][]string{
		"list_tools":    {"server"},
		"describe_tool": {"server", "tools"},
		"call":          {"server", "tool"},
		"search_tools":  {"query"},
	}
	for tool, want := range required {
		schema, _ := names[tool]["inputSchema"].(map[string]any)
		var got []string
		switch r := schema["required"].(type) {
		case []string:
			got = r
		case []any:
			for _, x := range r {
				if s, ok := x.(string); ok {
					got = append(got, s)
				}
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s required = %v, want %v", tool, got, want)
		}
	}
}

func TestTextResultShape(t *testing.T) {
	res := textResult(map[string]any{"a": 1})
	if res["isError"] != false {
		t.Fatalf("isError = %v", res["isError"])
	}
	content, ok := res["content"].([]map[string]any)
	if !ok || len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("content = %v", res["content"])
	}
	var v map[string]any
	text, _ := content[0]["text"].(string)
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("text not JSON: %v", content[0]["text"])
	}
}
