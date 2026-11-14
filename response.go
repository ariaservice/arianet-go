package arianet

import (
	"encoding/json"
	"net/http"
)

// Response is a successful API answer.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Decode unmarshals the JSON body into v.
func (r *Response) Decode(v any) error {
	if len(r.Body) == 0 {
		return nil
	}
	return json.Unmarshal(r.Body, v)
}

// DecodeData unmarshals the "data" member of the body into v. Core API
// answers wrap their payload as {"success": true, "data": ...}, and most
// product API lists use {"data": [...], "meta": ...}.
func (r *Response) DecodeData(v any) error {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := r.Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 {
		return nil
	}
	return json.Unmarshal(envelope.Data, v)
}
