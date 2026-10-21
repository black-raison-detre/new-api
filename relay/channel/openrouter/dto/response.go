package dto

import "encoding/json"

// EnterpriseResponse is kept separate from the adaptor so shared response
// handlers can decode it without depending on the OpenRouter adaptor package.
type EnterpriseResponse struct {
	Data    json.RawMessage `json:"data"`
	Success bool            `json:"success"`
}
