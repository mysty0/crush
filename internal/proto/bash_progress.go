package proto

// BashProgress carries incremental output from a running foreground bash
// command so the client TUI can render it live before the tool returns.
type BashProgress struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
}
