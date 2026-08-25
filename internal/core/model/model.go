package model

type FinalStatus string

const (
	FinalStatusCompleted FinalStatus = "completed"
	FinalStatusCancelled FinalStatus = "cancelled"
	FinalStatusUnset     FinalStatus = "unset"
)

type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
	ReasoningTokens   int64
}

type LLMCall struct {
	CallID          string
	StartUnixNano   int64
	EndUnixNano     int64
	Provider        string
	RequestModel    string
	ResponseModel   string
	InputMessages   any
	OutputMessages  any
	InputPreview    string
	OutputPreview   string
	OutputKind      string
	FinishReasons   []string
	Usage           Usage
	TTFTMs          float64
	Status          string
	ErrorType       string
	Reason          string
	ExtraAttributes map[string]any
}

type SkillUse struct {
	Name          string
	CallID        string
	Path          string
	SourceType    string
	Description   string
	Version       string
	InputPreview  string
	OutputPreview string
	Status        string
	ErrorType     string
	Reason        string
}

type ToolCall struct {
	CallID            string
	TriggeringLLMCall string
	Name              string
	StartUnixNano     int64
	EndUnixNano       int64
	Arguments         any
	Result            any
	Command           string
	ResultStatus      string
	Status            string
	ErrorType         string
	Reason            string
	Skill             *SkillUse
	InputPreview      string
	OutputPreview     string
	ExtraAttributes   map[string]any
}

type AssistantOutput struct {
	StartUnixNano   int64
	EndUnixNano     int64
	OutputMessages  any
	OutputPreview   string
	OutputKind      string
	Provider        string
	RequestModel    string
	ResponseModel   string
	Status          string
	ErrorType       string
	Reason          string
	ExtraAttributes map[string]any
}

type Turn struct {
	SessionID        string
	TurnID           string
	AgentRuntime     string
	AgentName        string
	AgentVersion     string
	StartUnixNano    int64
	EndUnixNano      int64
	FinalStatus      FinalStatus
	InputMessages    any
	OutputMessages   any
	InputPreview     string
	OutputPreview    string
	InputLength      int
	OutputLength     int
	Usage            Usage
	CreditUsage      float64
	LLMCalls         []LLMCall
	ToolCalls        []ToolCall
	AssistantOutputs []AssistantOutput
	Resource         map[string]any
	ExtraAttributes  map[string]any
	ErrorType        string
	Reason           string
}

type SpanStatus struct {
	Code    string
	Message string
}

type Scope struct {
	Name       string
	Version    string
	Attributes map[string]any
}

type Span struct {
	TraceID           string
	SpanID            string
	ParentID          string
	Name              string
	Kind              string
	StartTimeUnixNano string
	EndTimeUnixNano   string
	StartTime         string
	EndTime           string
	DurationMs        int64
	Status            SpanStatus
	Attributes        map[string]any
	Resource          map[string]any
	Scope             Scope
	Ingest            map[string]any
}

type Metric struct {
	Name              string
	Type              string
	Unit              string
	Description       string
	Value             float64
	Attributes        map[string]any
	Resource          map[string]any
	Scope             Scope
	StartTimeUnixNano string
	TimeUnixNano      string
}
