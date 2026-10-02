package proxy

import (
	"bytes"
	"encoding/json"
)

// message is the subset of a JSON-RPC 2.0 envelope the proxy needs to route a line.
// Everything it does not route on (params of a tools/call, result payloads) stays a
// json.RawMessage, so user-supplied strings are carried as opaque bytes and never
// interpreted.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (m *message) hasID() bool {
	return len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null"))
}

func (m *message) isRequest() bool      { return m.Method != "" && m.hasID() }
func (m *message) isNotification() bool { return m.Method != "" && !m.hasID() }
func (m *message) isResponse() bool     { return m.Method == "" && m.hasID() }

// response is what the cache stores: the body of a response without its id.
type response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func responseOf(m *message) response { return response{Result: m.Result, Error: m.Error} }

type outResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	response
}

type outRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// encode marshals without HTML escaping. encoding/json would otherwise rewrite
// '<', '>' and '&' inside RawMessage values as < etc. — equivalent JSON, but
// not the bytes the server sent.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// canonical renders a JSON value in one fixed form, so values can be compared even
// if a peer re-serialised them (key order, "A" vs "A"). Used to match response
// ids to requests — numbers and strings stay distinct, so the proxy's string ids
// never collide with a client's numbers — and to tell whether a cached answer changed.
func canonical(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := encode(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// protocolVersionOf reads the protocolVersion field of initialize params or result.
func protocolVersionOf(raw json.RawMessage) string {
	var v struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.ProtocolVersion
}
