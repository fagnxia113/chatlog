package temporalgraph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func OpenStore(workDir string) (*Store, error) {
	baseDir := filepath.Join(os.TempDir(), "chatlog_graph")
	if strings.TrimSpace(workDir) != "" {
		baseDir = filepath.Join(workDir, ".chatlog_graph")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(baseDir, "temporal_graph.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: dbPath}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS graph_source_records (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id TEXT NOT NULL,
	source_type TEXT NOT NULL,
	event_type TEXT NOT NULL DEFAULT '',
	talker TEXT NOT NULL DEFAULT '',
	talker_name TEXT NOT NULL DEFAULT '',
	sender TEXT NOT NULL DEFAULT '',
	sender_name TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	event_time INTEGER NOT NULL,
	metadata_json TEXT NOT NULL DEFAULT '{}',
	context_json TEXT NOT NULL DEFAULT '[]',
	participants_json TEXT NOT NULL DEFAULT '[]',
	entity_hints_json TEXT NOT NULL DEFAULT '[]',
	raw_json TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL DEFAULT 'pending',
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_graph_source_unique ON graph_source_records(source_type, source_id);
CREATE INDEX IF NOT EXISTS idx_graph_source_status ON graph_source_records(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_graph_source_time ON graph_source_records(event_time);

CREATE TABLE IF NOT EXISTS graph_entities (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	canonical_id INTEGER NOT NULL DEFAULT 0,
	canonical_key TEXT NOT NULL DEFAULT '',
	canonical_name TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	entity_type TEXT NOT NULL DEFAULT 'unknown',
	aliases_json TEXT NOT NULL DEFAULT '[]',
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	mentions INTEGER NOT NULL DEFAULT 1,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_graph_entity_unique ON graph_entities(name, entity_type);
CREATE INDEX IF NOT EXISTS idx_graph_entity_canonical ON graph_entities(canonical_key);
CREATE INDEX IF NOT EXISTS idx_graph_entity_name ON graph_entities(name);

CREATE TABLE IF NOT EXISTS graph_relations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	subject_entity_id INTEGER NOT NULL,
	object_entity_id INTEGER NOT NULL,
	predicate TEXT NOT NULL,
	canonical_key TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	confidence REAL NOT NULL DEFAULT 0,
	support_score REAL NOT NULL DEFAULT 0,
	verified TEXT NOT NULL DEFAULT 'unverified',
	conflict_group TEXT NOT NULL DEFAULT '',
	valid_from INTEGER NOT NULL,
	valid_to INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL,
	evidence_count INTEGER NOT NULL DEFAULT 1,
	updated_at INTEGER NOT NULL,
	UNIQUE(subject_entity_id, predicate, object_entity_id, status)
);
CREATE INDEX IF NOT EXISTS idx_graph_relation_nodes ON graph_relations(subject_entity_id, object_entity_id);
CREATE INDEX IF NOT EXISTS idx_graph_relation_time ON graph_relations(last_seen);

CREATE TABLE IF NOT EXISTS graph_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	event_type TEXT NOT NULL DEFAULT 'event',
	title TEXT NOT NULL,
	summary TEXT NOT NULL,
	actors_json TEXT NOT NULL DEFAULT '[]',
	targets_json TEXT NOT NULL DEFAULT '[]',
	event_time INTEGER NOT NULL,
	confidence REAL NOT NULL DEFAULT 0,
	source_record_id INTEGER NOT NULL,
	evidence TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_graph_event_time ON graph_events(event_time);
CREATE INDEX IF NOT EXISTS idx_graph_event_source ON graph_events(source_record_id);

CREATE TABLE IF NOT EXISTS graph_facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	fact_key TEXT NOT NULL,
	statement TEXT NOT NULL,
	canonical_statement TEXT NOT NULL DEFAULT '',
	change_type TEXT NOT NULL DEFAULT 'observed',
	status TEXT NOT NULL DEFAULT 'active',
	confidence REAL NOT NULL DEFAULT 0,
	support_score REAL NOT NULL DEFAULT 0,
	verified TEXT NOT NULL DEFAULT 'unverified',
	conflict_group TEXT NOT NULL DEFAULT '',
	valid_from INTEGER NOT NULL,
	valid_to INTEGER NOT NULL DEFAULT 0,
	source_record_id INTEGER NOT NULL,
	evidence TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_graph_fact_unique ON graph_facts(fact_key, status);
CREATE INDEX IF NOT EXISTS idx_graph_fact_time ON graph_facts(valid_from, valid_to);

CREATE TABLE IF NOT EXISTS graph_evidence (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_record_id INTEGER NOT NULL,
	target_type TEXT NOT NULL,
	target_id INTEGER NOT NULL,
	evidence TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_graph_evidence_target ON graph_evidence(target_type, target_id);

CREATE TABLE IF NOT EXISTS graph_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_type TEXT NOT NULL,
	status TEXT NOT NULL,
	cursor TEXT NOT NULL DEFAULT '',
	total INTEGER NOT NULL DEFAULT 0,
	processed INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS graph_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`
	_, err := s.db.Exec(schema)
	for _, stmt := range []string{
		`ALTER TABLE graph_source_records ADD COLUMN context_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE graph_source_records ADD COLUMN participants_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE graph_source_records ADD COLUMN entity_hints_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE graph_entities ADD COLUMN canonical_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE graph_entities ADD COLUMN canonical_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE graph_entities ADD COLUMN canonical_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE graph_relations ADD COLUMN canonical_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE graph_relations ADD COLUMN support_score REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE graph_relations ADD COLUMN verified TEXT NOT NULL DEFAULT 'unverified'`,
		`ALTER TABLE graph_relations ADD COLUMN conflict_group TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE graph_facts ADD COLUMN canonical_statement TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE graph_facts ADD COLUMN support_score REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE graph_facts ADD COLUMN verified TEXT NOT NULL DEFAULT 'unverified'`,
		`ALTER TABLE graph_facts ADD COLUMN conflict_group TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = s.db.Exec(stmt)
	}
	return err
}

func (s *Store) UpsertSource(rec SourceRecord) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if rec.EventTime.IsZero() {
		rec.EventTime = time.Now()
	}
	if rec.SourceID == "" {
		rec.SourceID = fmt.Sprintf("%s:%d:%x", rec.SourceType, rec.EventTime.UnixNano(), len(rec.Content))
	}
	meta, _ := json.Marshal(rec.Metadata)
	contextJSON, _ := json.Marshal(rec.Context)
	participantsJSON, _ := json.Marshal(rec.Participants)
	entityHintsJSON, _ := json.Marshal(rec.EntityHints)
	raw := strings.TrimSpace(rec.RawJSON)
	if raw == "" {
		raw = "{}"
	}
	res, err := s.db.Exec(`
INSERT INTO graph_source_records(source_id, source_type, event_type, talker, talker_name, sender, sender_name, title, content, event_time, metadata_json, context_json, participants_json, entity_hints_json, raw_json, status, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'pending', ?, ?)
ON CONFLICT(source_type, source_id) DO UPDATE SET
	talker=excluded.talker,
	talker_name=excluded.talker_name,
	sender=excluded.sender,
	sender_name=excluded.sender_name,
	title=excluded.title,
	content=excluded.content,
	event_time=excluded.event_time,
	metadata_json=excluded.metadata_json,
	context_json=excluded.context_json,
	participants_json=excluded.participants_json,
	entity_hints_json=excluded.entity_hints_json,
	raw_json=excluded.raw_json,
	status=CASE WHEN graph_source_records.status='done' AND graph_source_records.content=excluded.content THEN graph_source_records.status ELSE 'pending' END,
	error='',
	updated_at=excluded.updated_at
`, rec.SourceID, rec.SourceType, rec.EventType, rec.Talker, rec.TalkerName, rec.Sender, rec.SenderName, rec.Title, rec.Content, rec.EventTime.Unix(), string(meta), string(contextJSON), string(participantsJSON), string(entityHintsJSON), raw, now, now)
	if err != nil {
		return 0, false, err
	}
	id, _ := res.LastInsertId()
	inserted := id > 0
	if !inserted {
		row := s.db.QueryRow(`SELECT id FROM graph_source_records WHERE source_type=? AND source_id=?`, rec.SourceType, rec.SourceID)
		if err := row.Scan(&id); err != nil {
			return 0, false, err
		}
	}
	return id, inserted, nil
}

func (s *Store) PendingSources(limit int) ([]SourceRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, source_id, source_type, event_type, talker, talker_name, sender, sender_name, title, content, event_time, metadata_json, context_json, participants_json, entity_hints_json, raw_json, status, error, created_at, updated_at FROM graph_source_records WHERE status='pending' ORDER BY event_time ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *Store) ClaimPendingSources(limit int) ([]SourceRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, source_id, source_type, event_type, talker, talker_name, sender, sender_name, title, content, event_time, metadata_json, context_json, participants_json, entity_hints_json, raw_json, status, error, created_at, updated_at FROM graph_source_records WHERE status='pending' ORDER BY event_time ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	items, err := scanSources(rows)
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return items, tx.Commit()
	}
	ids := make([]any, 0, len(items))
	placeholders := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		placeholders = append(placeholders, "?")
	}
	now := time.Now().Unix()
	query := fmt.Sprintf(`UPDATE graph_source_records SET status='processing', error='', updated_at=? WHERE id IN (%s)`, strings.Join(placeholders, ","))
	args := append([]any{now}, ids...)
	if _, err := tx.Exec(query, args...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Status = "processing"
		items[i].Error = ""
	}
	return items, nil
}

func (s *Store) ResetProcessingSources() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE graph_source_records SET status='pending', error='', updated_at=? WHERE status='processing'`, time.Now().Unix())
	return err
}

func scanSources(rows *sql.Rows) ([]SourceRecord, error) {
	out := []SourceRecord{}
	for rows.Next() {
		var rec SourceRecord
		var ts int64
		var meta, contextJSON, participantsJSON, entityHintsJSON string
		if err := rows.Scan(&rec.ID, &rec.SourceID, &rec.SourceType, &rec.EventType, &rec.Talker, &rec.TalkerName, &rec.Sender, &rec.SenderName, &rec.Title, &rec.Content, &ts, &meta, &contextJSON, &participantsJSON, &entityHintsJSON, &rec.RawJSON, &rec.Status, &rec.Error, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		rec.EventTime = time.Unix(ts, 0)
		_ = json.Unmarshal([]byte(meta), &rec.Metadata)
		_ = json.Unmarshal([]byte(contextJSON), &rec.Context)
		_ = json.Unmarshal([]byte(participantsJSON), &rec.Participants)
		_ = json.Unmarshal([]byte(entityHintsJSON), &rec.EntityHints)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) MarkSource(id int64, status, errText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE graph_source_records SET status=?, error=?, updated_at=? WHERE id=?`, status, errText, time.Now().Unix(), id)
	return err
}

func (s *Store) ApplyExtraction(src SourceRecord, ext Extraction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	entityIDs := map[string]int64{}
	hints := entityHintsFromSource(src)
	for _, ent := range hints {
		id, err := upsertEntityTx(tx, ent, src.EventTime.Unix())
		if err != nil {
			return err
		}
		if id > 0 {
			entityIDs[entityKey(ent.Name, ent.Type)] = id
		}
	}
	for _, ent := range ext.Entities {
		id, err := upsertEntityTx(tx, ent, src.EventTime.Unix())
		if err != nil {
			return err
		}
		if id > 0 {
			entityIDs[entityKey(ent.Name, ent.Type)] = id
		}
	}
	for _, rel := range ext.Relations {
		subjID, err := ensureEntityTx(tx, rel.Subject, "unknown", src.EventTime.Unix())
		if err != nil {
			return err
		}
		objID, err := ensureEntityTx(tx, rel.Object, "unknown", src.EventTime.Unix())
		if err != nil {
			return err
		}
		if subjID == 0 || objID == 0 {
			continue
		}
		if id, err := upsertRelationTx(tx, subjID, objID, rel, src.EventTime.Unix()); err != nil {
			return err
		} else {
			_ = insertEvidenceTx(tx, src.ID, "relation", id, rel.Evidence)
		}
	}
	for _, ev := range ext.Events {
		if id, err := insertEventTx(tx, src, ev); err != nil {
			return err
		} else {
			_ = insertEvidenceTx(tx, src.ID, "event", id, ev.Evidence)
		}
	}
	for _, fact := range ext.Facts {
		if id, err := upsertFactTx(tx, src, fact); err != nil {
			return err
		} else {
			_ = insertEvidenceTx(tx, src.ID, "fact", id, fact.Evidence)
		}
	}
	for key := range entityIDs {
		_ = key
	}
	if _, err := tx.Exec(`UPDATE graph_source_records SET status='done', error='', updated_at=? WHERE id=?`, time.Now().Unix(), src.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertEntityTx(tx *sql.Tx, ent ExtractedEntity, ts int64) (int64, error) {
	name := cleanName(ent.Name)
	if name == "" {
		return 0, nil
	}
	typ := cleanType(ent.Type, "unknown")
	canonicalName := cleanName(ent.CanonicalName)
	if canonicalName == "" {
		canonicalName = canonicalEntityName(name)
	}
	canonicalKey := entityKey(canonicalName, typ)
	aliases, _ := json.Marshal(ent.Aliases)
	_, err := tx.Exec(`
INSERT INTO graph_entities(canonical_key, canonical_name, name, entity_type, aliases_json, first_seen, last_seen, mentions, updated_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(name, entity_type) DO UPDATE SET
	canonical_key=CASE WHEN canonical_key='' THEN excluded.canonical_key ELSE canonical_key END,
	canonical_name=CASE WHEN canonical_name='' THEN excluded.canonical_name ELSE canonical_name END,
	last_seen=MAX(last_seen, excluded.last_seen),
	mentions=mentions+1,
	aliases_json=CASE WHEN aliases_json='[]' THEN excluded.aliases_json ELSE aliases_json END,
	updated_at=excluded.updated_at
`, canonicalKey, canonicalName, name, typ, string(aliases), ts, ts, 1, ts)
	if err != nil {
		return 0, err
	}
	row := tx.QueryRow(`SELECT id FROM graph_entities WHERE name=? AND entity_type=?`, name, typ)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	canonicalID := id
	_ = tx.QueryRow(`SELECT id FROM graph_entities WHERE canonical_key=? ORDER BY mentions DESC, id ASC LIMIT 1`, canonicalKey).Scan(&canonicalID)
	_, _ = tx.Exec(`UPDATE graph_entities SET canonical_id=? WHERE canonical_key=?`, canonicalID, canonicalKey)
	return id, nil
}

func ensureEntityTx(tx *sql.Tx, name, typ string, ts int64) (int64, error) {
	name = cleanName(name)
	typ = cleanType(typ, "unknown")
	if name == "" {
		return 0, nil
	}
	if typ == "unknown" {
		var id int64
		err := tx.QueryRow(`SELECT id FROM graph_entities WHERE name=? ORDER BY CASE entity_type WHEN 'unknown' THEN 1 ELSE 0 END, mentions DESC LIMIT 1`, name).Scan(&id)
		if err == nil {
			_, _ = tx.Exec(`UPDATE graph_entities SET last_seen=MAX(last_seen, ?), mentions=mentions+1, updated_at=? WHERE id=?`, ts, ts, id)
			return id, nil
		}
	}
	return upsertEntityTx(tx, ExtractedEntity{Name: name, Type: typ, Confidence: 0.5}, ts)
}

func upsertRelationTx(tx *sql.Tx, subjID, objID int64, rel ExtractedRelation, ts int64) (int64, error) {
	predicate := canonicalPredicate(rel.Predicate)
	canonicalPred := canonicalPredicate(firstNonEmpty(rel.CanonicalPredicate, predicate))
	status := cleanType(rel.Status, "active")
	verified := cleanType(rel.Verified, "unverified")
	support := clampConfidence(rel.SupportScore)
	validFrom := ts
	if rel.ValidFrom > 0 {
		validFrom = rel.ValidFrom
	}
	validTo := int64(0)
	if rel.ValidTo > 0 {
		validTo = rel.ValidTo
	}
	canonicalKey := relationCanonicalKey(subjID, objID, canonicalPred)
	conflictGroup := strings.TrimSpace(rel.ConflictGroup)
	if conflictGroup == "" && (status == "conflict" || rel.ChangeType == "conflict") {
		conflictGroup = "relation:" + canonicalKey
	}
	if rel.ChangeType == "ended" || rel.ChangeType == "removed" {
		status = "ended"
		if validTo == 0 {
			validTo = validFrom
		}
	}
	if status == "ended" || status == "conflict" || rel.ChangeType == "ended" || rel.ChangeType == "updated" || rel.ChangeType == "conflict" {
		_, _ = tx.Exec(`UPDATE graph_relations SET status=?, valid_to=?, last_seen=MAX(last_seen, ?), updated_at=? WHERE subject_entity_id=? AND predicate=? AND object_entity_id=? AND status='active' AND valid_to=0`,
			status, validFrom, validFrom, ts, subjID, predicate, objID)
	}
	conf := clampConfidence(rel.Confidence)
	_, err := tx.Exec(`
INSERT INTO graph_relations(subject_entity_id, object_entity_id, predicate, canonical_key, status, confidence, support_score, verified, conflict_group, valid_from, valid_to, last_seen, evidence_count, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,?)
ON CONFLICT(subject_entity_id, predicate, object_entity_id, status) DO UPDATE SET
	confidence=MAX(confidence, excluded.confidence),
	support_score=MAX(support_score, excluded.support_score),
	verified=excluded.verified,
	conflict_group=CASE WHEN excluded.conflict_group!='' THEN excluded.conflict_group ELSE conflict_group END,
	canonical_key=CASE WHEN canonical_key='' THEN excluded.canonical_key ELSE canonical_key END,
	last_seen=MAX(last_seen, excluded.last_seen),
	evidence_count=evidence_count+1,
	updated_at=excluded.updated_at
`, subjID, objID, predicate, canonicalKey, status, conf, support, verified, conflictGroup, validFrom, validTo, validFrom, ts)
	if err != nil {
		return 0, err
	}
	row := tx.QueryRow(`SELECT id FROM graph_relations WHERE subject_entity_id=? AND predicate=? AND object_entity_id=? AND status=?`, subjID, predicate, objID, status)
	var id int64
	return id, row.Scan(&id)
}

func insertEventTx(tx *sql.Tx, src SourceRecord, ev ExtractedEvent) (int64, error) {
	title := strings.TrimSpace(ev.Title)
	if title == "" {
		title = firstLine(src.Title, src.Content)
	}
	summary := strings.TrimSpace(ev.Summary)
	if summary == "" {
		summary = truncateRunes(src.Content, 180)
	}
	eventTime := src.EventTime.Unix()
	if ev.EventTime > 0 {
		eventTime = ev.EventTime
	}
	actors, _ := json.Marshal(ev.Actors)
	targets, _ := json.Marshal(ev.Targets)
	res, err := tx.Exec(`INSERT INTO graph_events(event_type, title, summary, actors_json, targets_json, event_time, confidence, source_record_id, evidence, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		cleanType(ev.EventType, "event"), title, summary, string(actors), string(targets), eventTime, clampConfidence(ev.Confidence), src.ID, ev.Evidence, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func upsertFactTx(tx *sql.Tx, src SourceRecord, fact ExtractedFact) (int64, error) {
	statement := strings.TrimSpace(fact.Statement)
	if statement == "" {
		return 0, nil
	}
	canonicalStatement := strings.TrimSpace(fact.CanonicalStatement)
	if canonicalStatement == "" {
		canonicalStatement = canonicalFactStatement(statement)
	}
	key := factKey(canonicalStatement)
	status := cleanType(fact.Status, "active")
	changeType := cleanType(fact.ChangeType, "observed")
	verified := cleanType(fact.Verified, "unverified")
	support := clampConfidence(fact.SupportScore)
	validFrom := src.EventTime.Unix()
	if fact.ValidFrom > 0 {
		validFrom = fact.ValidFrom
	}
	validTo := int64(0)
	if fact.ValidTo > 0 {
		validTo = fact.ValidTo
	}
	conflictGroup := strings.TrimSpace(fact.ConflictGroup)
	if conflictGroup == "" && (status == "conflict" || changeType == "conflict") {
		conflictGroup = "fact:" + key
	}
	if status == "ended" || status == "conflict" || changeType == "ended" || changeType == "updated" || changeType == "conflict" {
		if validTo == 0 && status == "ended" {
			validTo = validFrom
		}
		_, _ = tx.Exec(`UPDATE graph_facts SET status=?, valid_to=?, updated_at=? WHERE fact_key=? AND status='active' AND valid_to=0`, status, validFrom, time.Now().Unix(), key)
	}
	res, err := tx.Exec(`
INSERT INTO graph_facts(fact_key, statement, canonical_statement, change_type, status, confidence, support_score, verified, conflict_group, valid_from, valid_to, source_record_id, evidence, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(fact_key, status) DO UPDATE SET
	confidence=MAX(confidence, excluded.confidence),
	support_score=MAX(support_score, excluded.support_score),
	verified=excluded.verified,
	conflict_group=CASE WHEN excluded.conflict_group!='' THEN excluded.conflict_group ELSE conflict_group END,
	canonical_statement=CASE WHEN canonical_statement='' THEN excluded.canonical_statement ELSE canonical_statement END,
	valid_from=MIN(valid_from, excluded.valid_from),
	source_record_id=excluded.source_record_id,
	evidence=excluded.evidence,
	updated_at=excluded.updated_at
`, key, statement, canonicalStatement, changeType, status, clampConfidence(fact.Confidence), support, verified, conflictGroup, validFrom, validTo, src.ID, fact.Evidence, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		row := tx.QueryRow(`SELECT id FROM graph_facts WHERE fact_key=? AND status=?`, key, status)
		_ = row.Scan(&id)
	}
	return id, nil
}

func insertEvidenceTx(tx *sql.Tx, srcID int64, targetType string, targetID int64, evidence string) error {
	_, err := tx.Exec(`INSERT INTO graph_evidence(source_record_id, target_type, target_id, evidence, created_at) VALUES(?,?,?,?,?)`, srcID, targetType, targetID, evidence, time.Now().Unix())
	return err
}

func (s *Store) Status(paused, running bool, lastErr string) Status {
	st := Status{Enabled: true, Paused: paused, Running: running, StorePath: s.path, LastError: lastErr}
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_entities`).Scan(&st.EntityCount)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_relations`).Scan(&st.RelationCount)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_events`).Scan(&st.EventCount)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_facts`).Scan(&st.FactCount)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_source_records`).Scan(&st.SourceCount)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_source_records WHERE status='pending'`).Scan(&st.Pending)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_source_records WHERE status='processing'`).Scan(&st.Processing)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_source_records WHERE status='done'`).Scan(&st.Processed)
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM graph_source_records WHERE status='failed'`).Scan(&st.Failed)
	var last int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(updated_at),0) FROM graph_source_records`).Scan(&last)
	if last > 0 {
		st.LastUpdatedAt = time.Unix(last, 0).Format(time.RFC3339)
	}
	return st
}

func (s *Store) Query(keyword, entity, relation string, start, end time.Time, limit int) (QueryResult, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	startTS, endTS := int64(0), int64(1<<62)
	if !start.IsZero() {
		startTS = start.Unix()
	}
	if !end.IsZero() {
		endTS = end.Unix()
	}
	kw := "%" + strings.TrimSpace(keyword) + "%"
	ent := "%" + strings.TrimSpace(entity) + "%"
	rel := "%" + strings.TrimSpace(relation) + "%"
	out := QueryResult{}
	rows, err := s.db.Query(`SELECT id, canonical_id, canonical_name, canonical_key, name, entity_type, aliases_json, first_seen, last_seen, mentions FROM graph_entities WHERE (?='%%' OR name LIKE ? OR aliases_json LIKE ? OR canonical_name LIKE ?) AND (?='%%' OR name LIKE ? OR canonical_name LIKE ?) ORDER BY last_seen DESC LIMIT ?`, kw, kw, kw, kw, ent, ent, ent, limit)
	if err != nil {
		return out, err
	}
	out.Entities, err = scanEntities(rows)
	if err != nil {
		return out, err
	}
	rows, err = s.db.Query(`
SELECT r.id, r.subject_entity_id, r.object_entity_id, se.name, oe.name, r.predicate, r.canonical_key, r.status, r.confidence, r.support_score, r.verified, r.conflict_group, r.valid_from, r.valid_to, r.last_seen, r.evidence_count
FROM graph_relations r
JOIN graph_entities se ON se.id=r.subject_entity_id
JOIN graph_entities oe ON oe.id=r.object_entity_id
WHERE r.last_seen BETWEEN ? AND ? AND (?='%%' OR se.name LIKE ? OR oe.name LIKE ? OR r.predicate LIKE ?) AND (?='%%' OR se.name LIKE ? OR oe.name LIKE ?) AND (?='%%' OR r.predicate LIKE ?)
ORDER BY r.last_seen DESC LIMIT ?`, startTS, endTS, kw, kw, kw, kw, ent, ent, ent, rel, rel, limit)
	if err != nil {
		return out, err
	}
	out.Relations, err = scanRelations(rows)
	if err != nil {
		return out, err
	}
	rows, err = s.db.Query(`SELECT e.id, e.event_type, e.title, e.summary, e.actors_json, e.targets_json, e.event_time, e.confidence, e.source_record_id, e.evidence, s.source_type || ':' || s.source_id FROM graph_events e LEFT JOIN graph_source_records s ON s.id=e.source_record_id WHERE e.event_time BETWEEN ? AND ? AND (?='%%' OR e.title LIKE ? OR e.summary LIKE ? OR e.actors_json LIKE ? OR e.targets_json LIKE ?) ORDER BY e.event_time DESC LIMIT ?`, startTS, endTS, kw, kw, kw, kw, kw, limit)
	if err != nil {
		return out, err
	}
	out.Events, err = scanEvents(rows)
	if err != nil {
		return out, err
	}
	rows, err = s.db.Query(`SELECT id, fact_key, statement, canonical_statement, change_type, status, confidence, support_score, verified, conflict_group, valid_from, valid_to, source_record_id, evidence FROM graph_facts WHERE valid_from BETWEEN ? AND ? AND (?='%%' OR statement LIKE ? OR canonical_statement LIKE ?) ORDER BY valid_from DESC LIMIT ?`, startTS, endTS, kw, kw, kw, limit)
	if err != nil {
		return out, err
	}
	out.Facts, err = scanFacts(rows)
	return out, err
}

func scanEntities(rows *sql.Rows) ([]GraphEntity, error) {
	defer rows.Close()
	out := []GraphEntity{}
	for rows.Next() {
		var e GraphEntity
		var aliases string
		if err := rows.Scan(&e.ID, &e.CanonicalID, &e.CanonicalName, &e.CanonicalKey, &e.Name, &e.Type, &aliases, &e.FirstSeen, &e.LastSeen, &e.Mentions); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(aliases), &e.Aliases)
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanRelations(rows *sql.Rows) ([]GraphRelation, error) {
	defer rows.Close()
	out := []GraphRelation{}
	for rows.Next() {
		var r GraphRelation
		if err := rows.Scan(&r.ID, &r.SubjectEntity, &r.ObjectEntity, &r.Subject, &r.Object, &r.Predicate, &r.CanonicalKey, &r.Status, &r.Confidence, &r.SupportScore, &r.Verified, &r.ConflictGroup, &r.ValidFrom, &r.ValidTo, &r.LastSeen, &r.EvidenceCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanEvents(rows *sql.Rows) ([]GraphEvent, error) {
	defer rows.Close()
	out := []GraphEvent{}
	for rows.Next() {
		var e GraphEvent
		var actors, targets string
		if err := rows.Scan(&e.ID, &e.EventType, &e.Title, &e.Summary, &actors, &targets, &e.EventTime, &e.Confidence, &e.SourceID, &e.Evidence, &e.SourceLabel); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(actors), &e.Actors)
		_ = json.Unmarshal([]byte(targets), &e.Targets)
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanFacts(rows *sql.Rows) ([]GraphFact, error) {
	defer rows.Close()
	out := []GraphFact{}
	for rows.Next() {
		var f GraphFact
		if err := rows.Scan(&f.ID, &f.FactKey, &f.Statement, &f.CanonicalStatement, &f.ChangeType, &f.Status, &f.Confidence, &f.SupportScore, &f.Verified, &f.ConflictGroup, &f.ValidFrom, &f.ValidTo, &f.SourceID, &f.Evidence); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Visualize(keyword string, start, end time.Time, limit int) (VisualizeResult, error) {
	q, err := s.Query(keyword, "", "", start, end, limit)
	if err != nil {
		return VisualizeResult{}, err
	}
	nodes := map[string]GraphNode{}
	for _, e := range q.Entities {
		nodes[fmt.Sprintf("entity:%d", e.ID)] = GraphNode{ID: fmt.Sprintf("entity:%d", e.ID), Name: e.Name, Kind: e.Type, Value: e.Mentions, LastSeen: e.LastSeen}
	}
	edges := make([]GraphEdge, 0, len(q.Relations))
	for _, r := range q.Relations {
		sid := fmt.Sprintf("entity:%d", r.SubjectEntity)
		oid := fmt.Sprintf("entity:%d", r.ObjectEntity)
		if _, ok := nodes[sid]; !ok {
			nodes[sid] = GraphNode{ID: sid, Name: r.Subject, Kind: "unknown", Value: 1, LastSeen: r.LastSeen}
		}
		if _, ok := nodes[oid]; !ok {
			nodes[oid] = GraphNode{ID: oid, Name: r.Object, Kind: "unknown", Value: 1, LastSeen: r.LastSeen}
		}
		edges = append(edges, GraphEdge{
			ID: fmt.Sprintf("relation:%d", r.ID), Source: sid, Target: oid, Label: r.Predicate,
			Status: r.Status, Confidence: r.Confidence, LastSeen: r.LastSeen, Evidence: r.EvidenceCount,
		})
	}
	timeline := make([]GraphTimeline, 0, len(q.Events)+len(q.Facts)+len(q.Relations))
	for _, e := range q.Events {
		timeline = append(timeline, GraphTimeline{Time: e.EventTime, Type: "event", Title: e.Title, Description: e.Summary, Source: e.SourceLabel})
	}
	for _, f := range q.Facts {
		timeline = append(timeline, GraphTimeline{Time: f.ValidFrom, Type: "fact", Title: f.ChangeType, Description: f.Statement})
	}
	for _, r := range q.Relations {
		timeline = append(timeline, GraphTimeline{Time: r.LastSeen, Type: "relation", Title: r.Predicate, Description: r.Subject + " -> " + r.Object})
	}
	outNodes := make([]GraphNode, 0, len(nodes))
	for _, n := range nodes {
		outNodes = append(outNodes, n)
	}
	return VisualizeResult{Nodes: outNodes, Edges: edges, Timeline: timeline, Generated: time.Now().Unix()}, nil
}

func (s *Store) ClearGraph() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM graph_entities; DELETE FROM graph_relations; DELETE FROM graph_events; DELETE FROM graph_facts; DELETE FROM graph_evidence; UPDATE graph_source_records SET status='pending', error='', updated_at=?`, time.Now().Unix())
	return err
}

func (s *Store) SetMeta(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO graph_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetMeta(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM graph_meta WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}
