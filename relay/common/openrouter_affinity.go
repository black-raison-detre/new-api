package common

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

const OpenRouterRoutingRetryStateContextKey = "openrouter_model_routing_retry_state"

type OpenRouterRoutingRetryState struct {
	ExposedModel string
	Policy       dto.OpenRouterModelRoutingPolicy
}

// OpenRouterConversationKey returns a non-reversible key for the caller's
// stable conversation identity. It never includes user or prompt text in the
// returned value or in logs.
func OpenRouterConversationKey(body []byte, xSessionID string) (string, error) {
	var fields map[string]json.RawMessage
	if len(body) > 0 {
		if err := rootcommon.Unmarshal(body, &fields); err != nil {
			return "", fmt.Errorf("invalid OpenRouter session request body")
		}
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}

	for _, candidate := range []struct {
		name  string
		value string
	}{
		{name: "session_id", value: sessionIDField(fields, "session_id")},
		{name: "x-session-id", value: xSessionID},
		{name: "prompt_cache_key", value: sessionIDField(fields, "prompt_cache_key")},
		{name: "conversation_id", value: sessionIDField(fields, "conversation_id")},
		{name: "conversation", value: conversationField(fields["conversation"])},
		{name: "metadata.conversation_id", value: metadataConversationID(fields["metadata"])},
	} {
		if candidate.value == "" {
			continue
		}
		if err := validateOpenRouterSessionIdentity(candidate.value, candidate.name); err != nil {
			return "", err
		}
		return openRouterDigest("conversation:v1", candidate.value), nil
	}

	opening, ok := openRouterOpeningInput(fields)
	if !ok {
		return "", nil
	}
	return openRouterDigest("opening-input:v1", opening), nil
}

func OpenRouterAffinityKey(userID int, model, group, conversationKey string) string {
	if userID <= 0 || model == "" || conversationKey == "" {
		return ""
	}
	return openRouterDigest("channel-affinity:v1", fmt.Sprintf("%d\x00%s\x00%s\x00%s", userID, group, model, conversationKey))
}

// OpenRouterAccountScope identifies channels that use the same upstream
// account key at the same normalized base URL. The API key is only hashed.
func OpenRouterAccountScope(baseURL, apiKey string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	keyFingerprint := openRouterDigest("account-key:v1", apiKey)
	return openRouterDigest("account-scope:v1", parsed.String()+"\x00"+keyFingerprint)
}

// OpenRouterSessionID is an upstream-safe, user-and-conversation-scoped
// session key. The channel credential is used as an HMAC key and is never
// included in the result; key rotation therefore starts a new affinity scope.
func OpenRouterSessionID(userID int, model, group, policy, accountScope, conversationKey, apiKey string) string {
	if userID <= 0 || model == "" || conversationKey == "" || accountScope == "" {
		return ""
	}
	identity := fmt.Sprintf("openrouter-session:v1\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s", userID, group, model, policy, accountScope, conversationKey)
	hasher := hmac.New(sha256.New, []byte(apiKey))
	_, _ = hasher.Write([]byte(identity))
	return "or_" + hex.EncodeToString(hasher.Sum(nil))
}

func sessionIDField(fields map[string]json.RawMessage, key string) string {
	value := fields[key]
	if len(value) == 0 || string(value) == "null" {
		return ""
	}
	var decoded string
	if err := rootcommon.Unmarshal(value, &decoded); err != nil {
		return "\x00invalid"
	}
	return decoded
}

func conversationField(value json.RawMessage) string {
	if len(value) == 0 || string(value) == "null" {
		return ""
	}
	var decoded string
	if err := rootcommon.Unmarshal(value, &decoded); err == nil {
		return decoded
	}
	var fields map[string]json.RawMessage
	if err := rootcommon.Unmarshal(value, &fields); err != nil {
		return "\x00invalid"
	}
	return sessionIDField(fields, "id")
}

func metadataConversationID(value json.RawMessage) string {
	if len(value) == 0 || string(value) == "null" {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := rootcommon.Unmarshal(value, &fields); err != nil {
		return "\x00invalid"
	}
	return sessionIDField(fields, "conversation_id")
}

func validateOpenRouterSessionIdentity(value, name string) error {
	if strings.HasPrefix(value, "\x00invalid") {
		return fmt.Errorf("invalid OpenRouter %s", name)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("invalid OpenRouter %s", name)
	}
	if len(value) > 256 {
		return fmt.Errorf("OpenRouter %s exceeds 256 bytes", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid OpenRouter %s", name)
		}
	}
	return nil
}

func openRouterOpeningInput(fields map[string]json.RawMessage) (string, bool) {
	var opening strings.Builder
	for _, key := range []string{"system", "system_instruction", "instructions"} {
		if value := fields[key]; len(value) > 0 && string(value) != "null" {
			opening.WriteString(key)
			opening.WriteByte(0)
			opening.Write(value)
			opening.WriteByte(0)
		}
	}
	for _, key := range []string{"messages", "contents", "input"} {
		value := fields[key]
		if len(value) == 0 || string(value) == "null" {
			continue
		}
		first, ok := firstOpenRouterMessage(value)
		if !ok {
			continue
		}
		opening.WriteString(key)
		opening.WriteByte(0)
		opening.Write(first)
		return opening.String(), true
	}
	if value := fields["prompt"]; len(value) > 0 && string(value) != "null" {
		opening.WriteString("prompt\x00")
		opening.Write(value)
		return opening.String(), true
	}
	return "", false
}

func firstOpenRouterMessage(value json.RawMessage) ([]byte, bool) {
	var items []json.RawMessage
	if err := rootcommon.Unmarshal(value, &items); err != nil || len(items) == 0 {
		if len(value) > 0 && value[0] == '"' {
			return value, true
		}
		return nil, false
	}
	var opening strings.Builder
	for _, item := range items {
		var fields map[string]json.RawMessage
		if err := rootcommon.Unmarshal(item, &fields); err != nil {
			continue
		}
		role := sessionIDField(fields, "role")
		if role == "system" || role == "developer" {
			opening.WriteString(role)
			opening.WriteByte(0)
			opening.Write(item)
			opening.WriteByte(0)
			continue
		}
		if role == "" {
			role = sessionIDField(fields, "type")
		}
		opening.WriteString(role)
		opening.WriteByte(0)
		opening.Write(item)
		return []byte(opening.String()), true
	}
	return nil, false
}

func openRouterDigest(domain, value string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(domain))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(value))
	return hex.EncodeToString(hasher.Sum(nil))
}

func SetOpenRouterRoutingRetryState(c *gin.Context, state OpenRouterRoutingRetryState) {
	if c == nil {
		return
	}
	c.Set(OpenRouterRoutingRetryStateContextKey, state)
}

func GetOpenRouterRoutingRetryState(c *gin.Context) (OpenRouterRoutingRetryState, bool) {
	if c == nil {
		return OpenRouterRoutingRetryState{}, false
	}
	value, exists := c.Get(OpenRouterRoutingRetryStateContextKey)
	state, ok := value.(OpenRouterRoutingRetryState)
	return state, exists && ok
}
