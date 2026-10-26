package kandev

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// JSON encodes an action's reply.
func JSON(value any) (*pluginsdk.PluginActionResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("kandev-plugin-forgejo: encode action response: %w", err)
	}
	return &pluginsdk.PluginActionResponse{
		Body:    body,
		Headers: map[string]string{"Content-Type": "application/json"},
	}, nil
}

// Refuse is an expected failure: a fixed message under an explicit status,
// never provider text. The host masks a Go error from an action as a 503, so
// anything an operator can act on has to be returned this way.
func Refuse(status int, message string) (*pluginsdk.PluginActionResponse, error) {
	response, err := JSON(map[string]string{"error": message})
	if err != nil {
		return nil, err
	}
	response.Status = status
	return response, nil
}

// DecodeBody reads an action body into into. It returns a ready 422 when the
// body is not valid JSON for it, and nil otherwise; an empty body is valid.
func DecodeBody(request *pluginsdk.PluginActionRequest, into any) *pluginsdk.PluginActionResponse {
	if len(request.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(request.Body, into); err != nil {
		response, _ := Refuse(http.StatusUnprocessableEntity, "The request body is not valid.")
		return response
	}
	return nil
}
