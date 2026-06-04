package temporalgraph

import "time"

type SourceRecord struct {
	ID           int64              `json:"id"`
	SourceID     string             `json:"source_id"`
	SourceType   string             `json:"source_type"`
	EventType    string             `json:"event_type"`
	Talker       string             `json:"talker,omitempty"`
	TalkerName   string             `json:"talker_name,omitempty"`
	Sender       string             `json:"sender,omitempty"`
	SenderName   string             `json:"sender_name,omitempty"`
	Title        string             `json:"title,omitempty"`
	Content      string             `json:"content"`
	EventTime    time.Time          `json:"event_time"`
	Metadata     map[string]string  `json:"metadata,omitempty"`
	Context      []ContextMessage   `json:"context,omitempty"`
	Participants []GraphParticipant `json:"participants,omitempty"`
	EntityHints  []string           `json:"entity_hints,omitempty"`
	RawJSON      string             `json:"-"`
	Status       string             `json:"status"`
	Error        string             `json:"error,omitempty"`
	CreatedAt    int64              `json:"created_at"`
	UpdatedAt    int64              `json:"updated_at"`
}

type ContextMessage struct {
	Seq     int64  `json:"seq"`
	Time    string `json:"time"`
	Sender  string `json:"sender"`
	Content string `json:"content"`
	Role    string `json:"role"`
}

type GraphParticipant struct {
	UserName    string   `json:"user_name"`
	DisplayName string   `json:"display_name"`
	Kind        string   `json:"kind"`
	Aliases     []string `json:"aliases,omitempty"`
}

type IngestMessage struct {
	SourceID     string             `json:"source_id"`
	Talker       string             `json:"talker"`
	TalkerName   string             `json:"talker_name"`
	Sender       string             `json:"sender"`
	SenderName   string             `json:"sender_name"`
	Time         string             `json:"time"`
	Type         int64              `json:"type"`
	Content      string             `json:"content"`
	Metadata     map[string]string  `json:"metadata"`
	Context      []ContextMessage   `json:"context"`
	Participants []GraphParticipant `json:"participants"`
}

type IngestBusiness struct {
	Source   string            `json:"source"`
	Type     string            `json:"type"`
	Time     string            `json:"time"`
	Title    string            `json:"title"`
	Content  string            `json:"content"`
	Entities []string          `json:"entities"`
	Metadata map[string]string `json:"metadata"`
}

type IngestEvent struct {
	EventType string            `json:"event_type"`
	Time      string            `json:"time"`
	Actors    []string          `json:"actors"`
	Targets   []string          `json:"targets"`
	Content   string            `json:"content"`
	Metadata  map[string]string `json:"metadata"`
}

type GraphEntity struct {
	ID            int64    `json:"id"`
	CanonicalID   int64    `json:"canonical_id"`
	CanonicalName string   `json:"canonical_name"`
	CanonicalKey  string   `json:"canonical_key"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Aliases       []string `json:"aliases,omitempty"`
	FirstSeen     int64    `json:"first_seen"`
	LastSeen      int64    `json:"last_seen"`
	Mentions      int      `json:"mentions"`
}

type GraphRelation struct {
	ID            int64   `json:"id"`
	SubjectEntity int64   `json:"subject_entity_id"`
	ObjectEntity  int64   `json:"object_entity_id"`
	Subject       string  `json:"subject"`
	Object        string  `json:"object"`
	Predicate     string  `json:"predicate"`
	CanonicalKey  string  `json:"canonical_key"`
	Status        string  `json:"status"`
	Confidence    float64 `json:"confidence"`
	SupportScore  float64 `json:"support_score"`
	Verified      string  `json:"verified"`
	ConflictGroup string  `json:"conflict_group,omitempty"`
	ValidFrom     int64   `json:"valid_from"`
	ValidTo       int64   `json:"valid_to,omitempty"`
	LastSeen      int64   `json:"last_seen"`
	EvidenceCount int     `json:"evidence_count"`
}

type GraphEvent struct {
	ID          int64    `json:"id"`
	EventType   string   `json:"event_type"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Actors      []string `json:"actors,omitempty"`
	Targets     []string `json:"targets,omitempty"`
	EventTime   int64    `json:"event_time"`
	Confidence  float64  `json:"confidence"`
	SourceID    int64    `json:"source_record_id"`
	Evidence    string   `json:"evidence,omitempty"`
	SourceLabel string   `json:"source_label,omitempty"`
}

type GraphFact struct {
	ID                 int64   `json:"id"`
	FactKey            string  `json:"fact_key"`
	Statement          string  `json:"statement"`
	CanonicalStatement string  `json:"canonical_statement"`
	ChangeType         string  `json:"change_type"`
	Status             string  `json:"status"`
	Confidence         float64 `json:"confidence"`
	SupportScore       float64 `json:"support_score"`
	Verified           string  `json:"verified"`
	ConflictGroup      string  `json:"conflict_group,omitempty"`
	ValidFrom          int64   `json:"valid_from"`
	ValidTo            int64   `json:"valid_to,omitempty"`
	SourceID           int64   `json:"source_record_id"`
	Evidence           string  `json:"evidence,omitempty"`
}

type Extraction struct {
	Entities  []ExtractedEntity   `json:"entities"`
	Relations []ExtractedRelation `json:"relations"`
	Events    []ExtractedEvent    `json:"events"`
	Facts     []ExtractedFact     `json:"facts"`
}

type ExtractedEntity struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Aliases       []string `json:"aliases"`
	CanonicalName string   `json:"canonical_name"`
	Confidence    float64  `json:"confidence"`
}

type ExtractedRelation struct {
	Subject            string  `json:"subject"`
	Predicate          string  `json:"predicate"`
	CanonicalPredicate string  `json:"canonical_predicate"`
	Object             string  `json:"object"`
	TimeText           string  `json:"time_text"`
	ValidFrom          int64   `json:"valid_from,omitempty"`
	ValidTo            int64   `json:"valid_to,omitempty"`
	ChangeType         string  `json:"change_type"`
	Status             string  `json:"status"`
	Confidence         float64 `json:"confidence"`
	SupportScore       float64 `json:"support_score"`
	Verified           string  `json:"verified"`
	ConflictGroup      string  `json:"conflict_group"`
	Evidence           string  `json:"evidence"`
}

type ExtractedEvent struct {
	EventType  string   `json:"event_type"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	TimeText   string   `json:"time_text"`
	EventTime  int64    `json:"event_time,omitempty"`
	Actors     []string `json:"actors"`
	Targets    []string `json:"targets"`
	Confidence float64  `json:"confidence"`
	Evidence   string   `json:"evidence"`
}

type ExtractedFact struct {
	Statement          string  `json:"statement"`
	CanonicalStatement string  `json:"canonical_statement"`
	TimeText           string  `json:"time_text"`
	ValidFrom          int64   `json:"valid_from,omitempty"`
	ValidTo            int64   `json:"valid_to,omitempty"`
	ChangeType         string  `json:"change_type"`
	Status             string  `json:"status"`
	Confidence         float64 `json:"confidence"`
	SupportScore       float64 `json:"support_score"`
	Verified           string  `json:"verified"`
	ConflictGroup      string  `json:"conflict_group"`
	Evidence           string  `json:"evidence"`
}

type Status struct {
	Enabled              bool    `json:"enabled"`
	Paused               bool    `json:"paused"`
	Running              bool    `json:"running"`
	HistoryQueued        bool    `json:"history_queued"`
	EnqueueRunning       bool    `json:"enqueue_running"`
	Workers              int     `json:"workers"`
	EnqueueWorkers       int     `json:"enqueue_workers"`
	StorePath            string  `json:"store_path"`
	EntityCount          int     `json:"entity_count"`
	RelationCount        int     `json:"relation_count"`
	EventCount           int     `json:"event_count"`
	FactCount            int     `json:"fact_count"`
	SourceCount          int     `json:"source_count"`
	Pending              int     `json:"pending"`
	Processing           int     `json:"processing"`
	Processed            int     `json:"processed"`
	Failed               int     `json:"failed"`
	ProgressPct          float64 `json:"progress_pct"`
	StartedAt            string  `json:"started_at,omitempty"`
	ProcessingRatePerMin float64 `json:"processing_rate_per_minute,omitempty"`
	EstimatedSecondsLeft int64   `json:"estimated_seconds_left,omitempty"`
	LastUpdatedAt        string  `json:"last_updated_at,omitempty"`
	LastError            string  `json:"last_error,omitempty"`
}

type QueryResult struct {
	Entities  []GraphEntity   `json:"entities"`
	Relations []GraphRelation `json:"relations"`
	Events    []GraphEvent    `json:"events"`
	Facts     []GraphFact     `json:"facts"`
}

type VisualizeResult struct {
	Nodes     []GraphNode     `json:"nodes"`
	Edges     []GraphEdge     `json:"edges"`
	Timeline  []GraphTimeline `json:"timeline"`
	Generated int64           `json:"generated_at"`
}

type GraphNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Value    int    `json:"value"`
	LastSeen int64  `json:"last_seen"`
}

type GraphEdge struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Label      string  `json:"label"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	LastSeen   int64   `json:"last_seen"`
	Evidence   int     `json:"evidence_count"`
}

type GraphTimeline struct {
	Time        int64  `json:"time"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
}
