package pi

import "encoding/json"

type ThinkingLevel string

const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// Response is a Pi RPC command response.
type Response struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// Event is a non-response Pi RPC stdout record.
type Event struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

type SessionState struct {
	Model                 *ModelInfo `json:"model"`
	ThinkingLevel         string     `json:"thinkingLevel"`
	IsStreaming           bool       `json:"isStreaming"`
	IsCompacting          bool       `json:"isCompacting"`
	SteeringMode          string     `json:"steeringMode"`
	FollowUpMode          string     `json:"followUpMode"`
	SessionFile           string     `json:"sessionFile"`
	SessionID             string     `json:"sessionId"`
	SessionName           string     `json:"sessionName"`
	AutoCompactionEnabled bool       `json:"autoCompactionEnabled"`
	MessageCount          int        `json:"messageCount"`
	PendingMessageCount   int        `json:"pendingMessageCount"`
}

type ModelInfo struct {
	Provider      string   `json:"provider"`
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	API           string   `json:"api,omitempty"`
	BaseURL       string   `json:"baseUrl,omitempty"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens,omitempty"`
}

type modelsData struct {
	Models []ModelInfo `json:"models"`
}

type newSessionData struct {
	Cancelled bool `json:"cancelled"`
}

type SessionStats struct {
	SessionFile   string          `json:"sessionFile"`
	SessionID     string          `json:"sessionId"`
	UserMessages  int             `json:"userMessages"`
	AssistantMsgs int             `json:"assistantMessages"`
	ToolCalls     int             `json:"toolCalls"`
	ToolResults   int             `json:"toolResults"`
	TotalMessages int             `json:"totalMessages"`
	Raw           json.RawMessage `json:"-"`
}
