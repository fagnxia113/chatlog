package temporalgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/chatlog/semantic"
	"github.com/sjzar/chatlog/internal/model"
)

const (
	realtimeScanInterval       = 3 * time.Second
	realtimeSessionScanLimit   = 200
	realtimeMessageScanLimit   = 100
	historySessionScanLimit    = 500
	historyMessageBatchSize    = 300
	contextBeforeCount         = 5
	contextAfterCount          = 2
	chunkMessageCount          = 40
	chunkMaxGap                = 30 * time.Minute
	defaultGraphWorkers        = 1
	defaultGraphEnqueueWorkers = 1
	maxGraphWorkers            = 12
)

var (
	errNoGraphResults = errors.New("temporal graph extraction produced no reliable graph results")
	mentionTextRe     = regexp.MustCompile(`@\S+`)
)

type Config interface {
	GetWorkDir() string
	GetSemanticConfig() *conf.SemanticConfig
}

type Manager struct {
	conf   Config
	db     *database.Service
	store  *Store
	client *semantic.Client

	mu             sync.Mutex
	paused         bool
	running        bool
	enqueuing      bool
	lastErr        string
	runStartedAt   time.Time
	runStartedDone int
	workers        int
	enqueueWorkers int
	wake           chan struct{}
	stop           chan struct{}
	stopOnce       sync.Once
}

func NewManager(cfg Config, db *database.Service) (*Manager, error) {
	store, err := OpenStore(cfg.GetWorkDir())
	if err != nil {
		return nil, err
	}
	m := &Manager{
		conf:           cfg,
		db:             db,
		store:          store,
		client:         semantic.NewClient(),
		wake:           make(chan struct{}, 1),
		stop:           make(chan struct{}),
		workers:        defaultGraphWorkers,
		enqueueWorkers: defaultGraphEnqueueWorkers,
	}
	m.loadWorkerConfig()
	_ = store.ResetProcessingSources()
	if paused, _ := store.GetMeta("paused"); paused == "1" {
		m.paused = true
	}
	go m.loop()
	return m, nil
}

func (m *Manager) loadWorkerConfig() {
	if m == nil || m.store == nil {
		return
	}
	if raw, _ := m.store.GetMeta("workers"); strings.TrimSpace(raw) != "" {
		var v int
		if _, err := fmt.Sscanf(raw, "%d", &v); err == nil {
			m.workers = clampInt(v, 1, maxGraphWorkers)
		}
	}
	if raw, _ := m.store.GetMeta("enqueue_workers"); strings.TrimSpace(raw) != "" {
		var v int
		if _, err := fmt.Sscanf(raw, "%d", &v); err == nil {
			m.enqueueWorkers = clampInt(v, 1, maxGraphWorkers)
		}
	}
}

func (m *Manager) SetWorkers(workers, enqueueWorkers int) error {
	if m == nil || m.store == nil {
		return fmt.Errorf("temporal graph unavailable")
	}
	workers = clampInt(workers, 1, maxGraphWorkers)
	enqueueWorkers = clampInt(enqueueWorkers, 1, maxGraphWorkers)
	m.mu.Lock()
	m.workers = workers
	m.enqueueWorkers = enqueueWorkers
	m.mu.Unlock()
	if err := m.store.SetMeta("workers", fmt.Sprintf("%d", workers)); err != nil {
		return err
	}
	if err := m.store.SetMeta("enqueue_workers", fmt.Sprintf("%d", enqueueWorkers)); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.stopOnce.Do(func() { close(m.stop) })
	if m.store != nil {
		return m.store.Close()
	}
	return nil
}

func (m *Manager) Status() Status {
	if m == nil || m.store == nil {
		return Status{Enabled: false, LastError: "temporal graph unavailable"}
	}
	m.mu.Lock()
	paused, running, enqueuing, lastErr := m.paused, m.running, m.enqueuing, m.lastErr
	runStartedAt, runStartedDone := m.runStartedAt, m.runStartedDone
	m.mu.Unlock()
	st := m.store.Status(paused, running, lastErr)
	st.EnqueueRunning = enqueuing
	st.Workers = m.workers
	st.EnqueueWorkers = m.enqueueWorkers
	if st.SourceCount > 0 {
		st.ProgressPct = float64(st.Processed+st.Failed) * 100 / float64(st.SourceCount)
		if st.ProgressPct < 0 {
			st.ProgressPct = 0
		}
		if st.ProgressPct > 100 {
			st.ProgressPct = 100
		}
	}
	if running && !runStartedAt.IsZero() {
		st.StartedAt = runStartedAt.Format(time.RFC3339)
		doneDelta := st.Processed + st.Failed - runStartedDone
		if doneDelta > 0 {
			elapsed := time.Since(runStartedAt).Seconds()
			if elapsed > 0 {
				ratePerSec := float64(doneDelta) / elapsed
				st.ProcessingRatePerMin = ratePerSec * 60
				remaining := st.Pending + st.Processing
				if remaining > 0 && ratePerSec > 0 {
					st.EstimatedSecondsLeft = int64(math.Ceil(float64(remaining) / ratePerSec))
				}
			}
		}
	}
	if queued, _ := m.store.GetMeta("history_queued"); queued == "1" {
		st.HistoryQueued = true
	}
	return st
}

func (m *Manager) Pause() error {
	m.mu.Lock()
	m.paused = true
	m.mu.Unlock()
	return m.store.SetMeta("paused", "1")
}

func (m *Manager) Resume() error {
	m.mu.Lock()
	m.paused = false
	m.mu.Unlock()
	if err := m.store.SetMeta("paused", "0"); err != nil {
		return err
	}
	m.signal()
	return nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	ticker := time.NewTicker(realtimeScanInterval)
	defer ticker.Stop()
	m.EnsureHistoryQueued(context.Background())
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.EnsureHistoryQueued(context.Background())
			m.ProcessPending(context.Background(), 10)
		case <-m.wake:
			m.ProcessPending(context.Background(), 20)
		}
	}
}

func (m *Manager) ProcessPending(ctx context.Context, limit int) {
	if m == nil || m.store == nil {
		return
	}
	if !m.chatReady() {
		m.setError(fmt.Errorf("chat model is not configured"))
		return
	}
	m.mu.Lock()
	if m.paused || m.running {
		m.mu.Unlock()
		return
	}
	startStatus := m.store.Status(m.paused, false, "")
	m.running = true
	m.runStartedAt = time.Now()
	m.runStartedDone = startStatus.Processed + startStatus.Failed
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.runStartedAt = time.Time{}
		m.runStartedDone = 0
		m.mu.Unlock()
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		paused := m.paused
		m.mu.Unlock()
		if paused {
			return
		}
		workers := clampInt(m.workers, 1, maxGraphWorkers)
		batchLimit := limit
		if batchLimit < workers*2 {
			batchLimit = workers * 2
		}
		items, err := m.store.ClaimPendingSources(batchLimit)
		if err != nil {
			m.setError(err)
			return
		}
		if len(items) == 0 {
			return
		}
		m.processBatch(ctx, items, workers)
	}
}

func (m *Manager) processBatch(ctx context.Context, items []SourceRecord, workers int) {
	if len(items) == 0 {
		return
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}
	jobs := make(chan SourceRecord)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rec := range jobs {
				if ctx.Err() != nil {
					_ = m.store.MarkSource(rec.ID, "pending", "")
					continue
				}
				if shouldSkipGraphSource(rec) {
					_ = m.store.MarkSource(rec.ID, "done", "")
					m.setError(nil)
					continue
				}
				if err := m.processOne(ctx, rec); err != nil {
					if errors.Is(err, errNoGraphResults) {
						_ = m.store.MarkSource(rec.ID, "done", "")
						m.setError(nil)
						continue
					}
					m.setError(err)
					_ = m.store.MarkSource(rec.ID, "failed", err.Error())
					continue
				}
				m.setError(nil)
			}
		}()
	}
	for _, rec := range items {
		select {
		case <-ctx.Done():
			_ = m.store.MarkSource(rec.ID, "pending", "")
		case jobs <- rec:
		}
	}
	close(jobs)
	wg.Wait()
}

func (m *Manager) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastErr = ""
		return
	}
	m.lastErr = err.Error()
}

func (m *Manager) processOne(ctx context.Context, rec SourceRecord) error {
	ext, err := m.extract(ctx, rec)
	if err != nil {
		log.Debug().Err(err).Msg("temporal graph llm extract failed")
		return err
	}
	if len(ext.Entities) == 0 && len(ext.Events) == 0 && len(ext.Facts) == 0 && len(ext.Relations) == 0 {
		return errNoGraphResults
	}
	ext = normalizeExtraction(ext, rec)
	verified, err := m.verifyExtraction(ctx, rec, ext)
	if err != nil {
		log.Debug().Err(err).Msg("temporal graph llm verify failed")
		return err
	}
	verified = normalizeExtraction(verified, rec)
	if len(verified.Entities) == 0 && len(verified.Events) == 0 && len(verified.Facts) == 0 && len(verified.Relations) == 0 {
		return errNoGraphResults
	}
	return m.store.ApplyExtraction(rec, verified)
}

func (m *Manager) extract(ctx context.Context, rec SourceRecord) (Extraction, error) {
	cfgPtr := m.conf.GetSemanticConfig()
	if cfgPtr == nil {
		return Extraction{}, fmt.Errorf("semantic config missing")
	}
	cfg := conf.NormalizeSemanticConfig(*cfgPtr)
	if !cfg.Enabled || !conf.SemanticChatReady(cfg) {
		return Extraction{}, fmt.Errorf("chat model is not configured")
	}
	system := `你是时间知识图谱抽取器。请只输出 JSON，不要解释。
Schema:
{
  "entities":[{"name":"实体名","type":"person|organization|project|product|customer|group|topic|keyword|event|unknown","aliases":["别名"],"confidence":0.0}],
  "relations":[{"subject":"实体A","predicate":"关系英文或短中文","object":"实体B","time_text":"原文中的时间表达","change_type":"observed|created|updated|ended|conflict","status":"active|ended|conflict","confidence":0.0,"evidence":"原文证据"}],
  "events":[{"event_type":"事件类型","title":"标题","summary":"摘要","time_text":"原文中的时间表达","actors":["参与方"],"targets":["对象"],"confidence":0.0,"evidence":"原文证据"}],
  "facts":[{"statement":"可追溯事实陈述","time_text":"原文中的时间表达","change_type":"observed|created|updated|ended|conflict","status":"active|ended|conflict","confidence":0.0,"evidence":"原文证据"}]
}
要求：实体名优先使用 participants/entity_hints 中的可识别名称；context 是理解前后文的证据，target 或 content 是当前抽取重点；关系和事实必须能从 content/context 中直接得到；如果出现“今天/明天/下周/月底”等时间表达，原样写入 time_text；如果出现“取消/结束/不再/改为/变更/冲突”等表达，设置 change_type/status；不确定时降低 confidence。`
	raw, err := m.client.Chat(ctx, cfg, []semantic.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: toJSONString(sourcePromptPayload(rec))},
	})
	if err != nil {
		return Extraction{}, err
	}
	ext, err := decodeExtraction(raw)
	if err != nil {
		return Extraction{}, fmt.Errorf("decode graph extraction failed: %w; raw=%s", err, truncateRunes(raw, 260))
	}
	return ext, nil
}

func (m *Manager) verifyExtraction(ctx context.Context, rec SourceRecord, ext Extraction) (Extraction, error) {
	cfgPtr := m.conf.GetSemanticConfig()
	if cfgPtr == nil {
		return Extraction{}, fmt.Errorf("semantic config missing")
	}
	cfg := conf.NormalizeSemanticConfig(*cfgPtr)
	if !cfg.Enabled || !conf.SemanticChatReady(cfg) {
		return Extraction{}, fmt.Errorf("chat model is not configured")
	}
	system := `你是时间知识图谱质量校验器。请只输出 JSON，不要解释。
输入包含 source 和 extraction。你需要：
1. 删除 evidence 不支持、纯猜测或低价值的 facts/relations/events。
2. 为保留项补充 canonical_statement 或 canonical_predicate，用于语义等价合并。
3. 为保留项设置 verified: supported|partial|unsupported，support_score: 0-1。
4. 对冲突事实或关系设置 status/conflict_group；同一冲突组使用稳定短字符串。
5. 保留 entities，并为明显同一实体补 canonical_name。
输出仍使用 extraction JSON schema。unsupported 项不要输出。`
	payload := map[string]any{
		"source":     sourcePromptPayload(rec),
		"extraction": ext,
	}
	raw, err := m.client.Chat(ctx, cfg, []semantic.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: toJSONString(payload)},
	})
	if err != nil {
		return Extraction{}, err
	}
	out, err := decodeExtraction(raw)
	if err != nil {
		return Extraction{}, fmt.Errorf("decode graph verification failed: %w; raw=%s", err, truncateRunes(raw, 260))
	}
	return out, nil
}

func normalizeExtraction(ext Extraction, rec SourceRecord) Extraction {
	aliasMap := entityAliasMap(rec)
	added := map[string]struct{}{}
	outEntities := make([]ExtractedEntity, 0, len(ext.Entities)+4)
	addEntity := func(e ExtractedEntity) {
		e.Name = cleanName(e.Name)
		e.Type = cleanType(e.Type, "unknown")
		if canonical, ok := aliasMap[canonicalAliasKey(e.Name)]; ok {
			e.CanonicalName = canonical
		}
		if e.CanonicalName == "" {
			e.CanonicalName = canonicalEntityName(e.Name)
		}
		if e.Name == "" {
			return
		}
		key := entityKey(e.Name, e.Type)
		if _, ok := added[key]; ok {
			return
		}
		added[key] = struct{}{}
		e.Confidence = clampConfidence(e.Confidence)
		outEntities = append(outEntities, e)
	}
	for _, e := range ext.Entities {
		addEntity(e)
	}
	addEntity(ExtractedEntity{Name: rec.TalkerName, Type: "conversation", Confidence: 0.7})
	addEntity(ExtractedEntity{Name: rec.SenderName, Type: "person", Confidence: 0.7})
	for i := range ext.Relations {
		ext.Relations[i].Subject = canonicalizeEntityRef(ext.Relations[i].Subject, aliasMap)
		ext.Relations[i].Object = canonicalizeEntityRef(ext.Relations[i].Object, aliasMap)
		ext.Relations[i].Predicate = canonicalPredicate(ext.Relations[i].Predicate)
		ext.Relations[i].CanonicalPredicate = canonicalPredicate(firstNonEmpty(ext.Relations[i].CanonicalPredicate, ext.Relations[i].Predicate))
		ext.Relations[i].Status = cleanType(ext.Relations[i].Status, "active")
		ext.Relations[i].ChangeType = cleanType(ext.Relations[i].ChangeType, "observed")
		ext.Relations[i].Verified = cleanType(ext.Relations[i].Verified, "supported")
		ext.Relations[i].Confidence = clampConfidence(ext.Relations[i].Confidence)
		ext.Relations[i].SupportScore = clampConfidence(ext.Relations[i].SupportScore)
		if ext.Relations[i].ValidFrom <= 0 {
			ext.Relations[i].ValidFrom = resolveRelativeTime(rec.EventTime, ext.Relations[i].TimeText, ext.Relations[i].Evidence, rec.Content).Unix()
		}
		if ext.Relations[i].Evidence == "" {
			ext.Relations[i].Evidence = truncateRunes(rec.Content, 240)
		}
	}
	filteredRelations := ext.Relations[:0]
	for _, rel := range ext.Relations {
		if rel.Subject == "" || rel.Object == "" || rel.Verified == "unsupported" || rel.SupportScore < 0.35 {
			continue
		}
		filteredRelations = append(filteredRelations, rel)
	}
	ext.Relations = filteredRelations
	for i := range ext.Events {
		ext.Events[i].EventType = cleanType(ext.Events[i].EventType, rec.SourceType)
		if strings.TrimSpace(ext.Events[i].Title) == "" {
			ext.Events[i].Title = firstLine(rec.Title, rec.Content)
		}
		ext.Events[i].Actors = canonicalizeEntityRefs(ext.Events[i].Actors, aliasMap)
		ext.Events[i].Targets = canonicalizeEntityRefs(ext.Events[i].Targets, aliasMap)
		if ext.Events[i].EventTime <= 0 {
			ext.Events[i].EventTime = resolveRelativeTime(rec.EventTime, ext.Events[i].TimeText, ext.Events[i].Evidence, ext.Events[i].Summary, rec.Content).Unix()
		}
		ext.Events[i].Confidence = clampConfidence(ext.Events[i].Confidence)
	}
	for i := range ext.Facts {
		ext.Facts[i].Statement = strings.TrimSpace(ext.Facts[i].Statement)
		if strings.TrimSpace(ext.Facts[i].CanonicalStatement) == "" {
			ext.Facts[i].CanonicalStatement = canonicalFactStatement(ext.Facts[i].Statement)
		}
		ext.Facts[i].ChangeType = cleanType(ext.Facts[i].ChangeType, "observed")
		ext.Facts[i].Status = cleanType(ext.Facts[i].Status, "active")
		ext.Facts[i].Verified = cleanType(ext.Facts[i].Verified, "supported")
		ext.Facts[i].Confidence = clampConfidence(ext.Facts[i].Confidence)
		ext.Facts[i].SupportScore = clampConfidence(ext.Facts[i].SupportScore)
		if ext.Facts[i].ValidFrom <= 0 {
			ext.Facts[i].ValidFrom = resolveRelativeTime(rec.EventTime, ext.Facts[i].TimeText, ext.Facts[i].Evidence, ext.Facts[i].Statement, rec.Content).Unix()
		}
	}
	filteredFacts := ext.Facts[:0]
	for _, fact := range ext.Facts {
		if fact.Statement == "" || fact.Verified == "unsupported" || fact.SupportScore < 0.35 {
			continue
		}
		filteredFacts = append(filteredFacts, fact)
	}
	ext.Facts = filteredFacts
	ext.Entities = outEntities
	return ext
}

func entityAliasMap(rec SourceRecord) map[string]string {
	out := map[string]string{}
	add := func(canonical string, aliases ...string) {
		canonical = cleanName(canonical)
		if canonical == "" {
			return
		}
		for _, alias := range aliases {
			key := canonicalAliasKey(alias)
			if key != "" {
				out[key] = canonical
			}
		}
		out[canonicalAliasKey(canonical)] = canonical
	}
	add(rec.TalkerName, rec.Talker, rec.TalkerName)
	add(rec.SenderName, rec.Sender, rec.SenderName)
	for _, p := range rec.Participants {
		canonical := firstNonEmpty(p.DisplayName, p.UserName)
		aliases := append([]string{p.UserName, p.DisplayName}, p.Aliases...)
		add(canonical, aliases...)
	}
	for _, hint := range rec.EntityHints {
		add(hint, hint)
	}
	return out
}

func canonicalAliasKey(raw string) string {
	raw = canonicalEntityName(raw)
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	return raw
}

func canonicalizeEntityRef(raw string, aliases map[string]string) string {
	name := cleanName(raw)
	if name == "" {
		return ""
	}
	if canonical, ok := aliases[canonicalAliasKey(name)]; ok {
		return canonical
	}
	return name
}

func canonicalizeEntityRefs(items []string, aliases map[string]string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		name := canonicalizeEntityRef(item, aliases)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	return out
}

func firstNonEmpty(items ...string) string {
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			return item
		}
	}
	return ""
}

func (m *Manager) IngestMessage(ctx context.Context, msg IngestMessage) (int64, error) {
	rec := SourceRecord{
		SourceID: msg.SourceID, SourceType: "message", EventType: fmt.Sprintf("message_%d", msg.Type),
		Talker: msg.Talker, TalkerName: msg.TalkerName, Sender: msg.Sender, SenderName: msg.SenderName,
		Content: msg.Content, EventTime: parseTimeFlexible(msg.Time), Metadata: msg.Metadata,
		Context: msg.Context, Participants: msg.Participants,
	}
	if rec.SourceID == "" {
		rec.SourceID = fmt.Sprintf("%s:%s:%d", rec.Talker, rec.Sender, rec.EventTime.UnixNano())
	}
	raw, _ := json.Marshal(msg)
	rec.RawJSON = string(raw)
	id, _, err := m.store.UpsertSource(rec)
	if err == nil {
		m.signal()
	}
	return id, err
}

func (m *Manager) IngestBusiness(ctx context.Context, item IngestBusiness) (int64, error) {
	meta := item.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	if len(item.Entities) > 0 {
		meta["entities"] = strings.Join(item.Entities, ",")
	}
	rec := SourceRecord{
		SourceID:   strings.TrimSpace(item.Source + ":" + item.Type + ":" + item.Time + ":" + item.Title),
		SourceType: "business", EventType: item.Type, Title: item.Title, Content: item.Content,
		EventTime: parseTimeFlexible(item.Time), Metadata: meta, EntityHints: item.Entities,
	}
	raw, _ := json.Marshal(item)
	rec.RawJSON = string(raw)
	id, _, err := m.store.UpsertSource(rec)
	if err == nil {
		m.signal()
	}
	return id, err
}

func (m *Manager) IngestEvent(ctx context.Context, item IngestEvent) (int64, error) {
	meta := item.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	meta["actors"] = strings.Join(item.Actors, ",")
	meta["targets"] = strings.Join(item.Targets, ",")
	hints := append([]string{}, item.Actors...)
	hints = append(hints, item.Targets...)
	rec := SourceRecord{
		SourceID:   strings.TrimSpace(item.EventType + ":" + item.Time + ":" + item.Content),
		SourceType: "event", EventType: item.EventType, Content: item.Content,
		EventTime: parseTimeFlexible(item.Time), Metadata: meta, EntityHints: hints,
	}
	raw, _ := json.Marshal(item)
	rec.RawJSON = string(raw)
	id, _, err := m.store.UpsertSource(rec)
	if err == nil {
		m.signal()
	}
	return id, err
}

func (m *Manager) Rebuild(ctx context.Context, reset bool) error {
	if !m.chatReady() {
		return fmt.Errorf("chat model is not configured")
	}
	if reset {
		if err := m.store.ClearGraph(); err != nil {
			return err
		}
	}
	if m.db == nil {
		return fmt.Errorf("database unavailable")
	}
	go m.enqueueAllMessages(context.Background(), true)
	return nil
}

func (m *Manager) chatReady() bool {
	if m == nil || m.conf == nil {
		return false
	}
	cfg := m.conf.GetSemanticConfig()
	return cfg != nil && cfg.Enabled && conf.SemanticChatReady(*cfg)
}

func (m *Manager) EnsureHistoryQueued(ctx context.Context) {
	if m == nil || m.store == nil {
		return
	}
	if queued, _ := m.store.GetMeta("history_queued"); queued == "1" {
		m.EnqueueRecentMessages(ctx)
		return
	}
	go m.enqueueAllMessages(ctx, true)
}

func (m *Manager) EnqueueRecentMessages(ctx context.Context) {
	if m == nil || m.db == nil || m.store == nil {
		return
	}
	m.mu.Lock()
	paused := m.paused
	m.mu.Unlock()
	if paused {
		return
	}
	sessions, err := m.db.GetSessions("", realtimeSessionScanLimit, 0)
	if err != nil || sessions == nil || len(sessions.Items) == 0 {
		if err != nil {
			log.Debug().Err(err).Msg("temporal graph realtime session scan failed")
		}
		return
	}
	now := time.Now()
	anyQueued := false
	for _, sess := range sessions.Items {
		if ctx.Err() != nil {
			return
		}
		if sess == nil || strings.TrimSpace(sess.UserName) == "" {
			continue
		}
		lastSeq := m.lastMessageSeq(sess.UserName)
		if lastSeq == 0 {
			continue
		}
		msgs, err := m.db.GetMessages(time.Unix(0, 0), now.Add(time.Minute), sess.UserName, "", "", realtimeMessageScanLimit, 0)
		if err != nil {
			log.Debug().Err(err).Str("talker", sess.UserName).Msg("temporal graph realtime message scan failed")
			continue
		}
		participants := participantsForMessages(msgs)
		maxSeq := lastSeq
		for _, msg := range msgs {
			if msg == nil {
				continue
			}
			if msg.Seq > maxSeq {
				maxSeq = msg.Seq
			}
			if lastSeq > 0 && msg.Seq <= lastSeq {
				continue
			}
			content := strings.TrimSpace(msg.PlainTextContent())
			if content == "" {
				content = strings.TrimSpace(msg.Content)
			}
			if content == "" {
				continue
			}
			if shouldSkipGraphMessageContent(content, msg.Type) {
				continue
			}
			if _, _, err := m.store.UpsertSource(messageToSource(msg, msgs, participants)); err != nil {
				log.Debug().Err(err).Str("talker", msg.Talker).Int64("seq", msg.Seq).Msg("temporal graph realtime enqueue failed")
				continue
			}
			anyQueued = true
		}
		if maxSeq > lastSeq {
			_ = m.store.SetMeta(messageSeqMetaKey(sess.UserName), fmt.Sprintf("%d", maxSeq))
		}
	}
	if anyQueued {
		m.signal()
	}
}

func (m *Manager) lastMessageSeq(talker string) int64 {
	raw, err := m.store.GetMeta(messageSeqMetaKey(talker))
	if err != nil || strings.TrimSpace(raw) == "" {
		return 0
	}
	var seq int64
	_, _ = fmt.Sscanf(raw, "%d", &seq)
	return seq
}

func messageSeqMetaKey(talker string) string {
	return "message_seq:" + talker
}

func (m *Manager) enqueueAllMessages(ctx context.Context, markComplete bool) {
	if m == nil || m.db == nil || m.store == nil {
		return
	}
	m.mu.Lock()
	if m.enqueuing || m.paused {
		m.mu.Unlock()
		return
	}
	m.enqueuing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.enqueuing = false
		m.mu.Unlock()
	}()
	if err := m.enqueueHistory(ctx); err != nil {
		if isDatabaseNotReady(err) {
			m.setError(nil)
			log.Debug().Err(err).Msg("temporal graph history enqueue deferred until database is ready")
			return
		}
		m.setError(err)
		log.Debug().Err(err).Msg("temporal graph history enqueue failed")
		return
	}
	if markComplete {
		_ = m.store.SetMeta("history_queued", "1")
		_ = m.store.SetMeta("history_queued_at", time.Now().Format(time.RFC3339))
	}
	m.signal()
}

func (m *Manager) enqueueHistory(ctx context.Context) error {
	sessions, err := m.db.GetSessions("", historySessionScanLimit, 0)
	if err != nil {
		return err
	}
	if sessions == nil {
		return nil
	}
	talkers := make(chan string)
	workers := clampInt(m.enqueueWorkers, 1, maxGraphWorkers)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	setFirstErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for talker := range talkers {
				if ctx.Err() != nil {
					setFirstErr(ctx.Err())
					continue
				}
				if err := m.enqueueTalkerHistory(ctx, talker); err != nil {
					setFirstErr(err)
					log.Debug().Err(err).Str("talker", talker).Msg("temporal graph talker history enqueue failed")
				}
			}
		}()
	}
	for _, sess := range sessions.Items {
		if ctx.Err() != nil {
			break
		}
		if sess == nil || strings.TrimSpace(sess.UserName) == "" {
			continue
		}
		talkers <- sess.UserName
	}
	close(talkers)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (m *Manager) enqueueTalkerHistory(ctx context.Context, talker string) error {
	end := time.Now().Add(time.Minute)
	start := time.Unix(0, 0)
	maxSeq := m.lastMessageSeq(talker)
	offset := 0
	allMsgs := make([]*model.Message, 0, historyMessageBatchSize)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msgs, err := m.db.GetMessages(start, end, talker, "", "", historyMessageBatchSize, offset)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			break
		}
		for _, msg := range msgs {
			if msg != nil {
				allMsgs = append(allMsgs, msg)
			}
		}
		if len(msgs) < historyMessageBatchSize {
			break
		}
		offset += len(msgs)
	}
	participants := participantsForMessages(allMsgs)
	for _, msg := range allMsgs {
		if msg == nil {
			continue
		}
		content := messageGraphText(msg)
		if shouldSkipGraphMessageContent(content, msg.Type) {
			continue
		}
		_, _, _ = m.store.UpsertSource(messageToSource(msg, allMsgs, participants))
		if msg.Seq > maxSeq {
			maxSeq = msg.Seq
		}
	}
	m.enqueueSessionChunks(talker, allMsgs, participants)
	if maxSeq > 0 {
		_ = m.store.SetMeta(messageSeqMetaKey(talker), fmt.Sprintf("%d", maxSeq))
	}
	return nil
}

func messageToSource(msg *model.Message, scope []*model.Message, participants []GraphParticipant) SourceRecord {
	content := messageGraphText(msg)
	meta := map[string]string{
		"msg_type": fmt.Sprintf("%d", msg.Type), "msg_sub_type": fmt.Sprintf("%d", msg.SubType),
	}
	return SourceRecord{
		SourceID: fmt.Sprintf("%s:%d", msg.Talker, msg.Seq), SourceType: "message", EventType: fmt.Sprintf("message_%d", msg.Type),
		Talker: msg.Talker, TalkerName: msg.TalkerName, Sender: msg.Sender, SenderName: msg.SenderName,
		Content: content, EventTime: msg.Time, Metadata: meta,
		Context: contextForMessage(msg, scope), Participants: participants,
	}
}

func messageGraphText(msg *model.Message) string {
	if msg == nil {
		return ""
	}
	content := strings.TrimSpace(msg.PlainTextContent())
	if content == "" {
		content = strings.TrimSpace(msg.Content)
	}
	return content
}

func shouldSkipGraphMessageContent(content string, msgType int64) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return true
	}
	compact := strings.ToLower(strings.Join(strings.Fields(content), ""))
	if strings.Contains(content, "撤回了一条消息") {
		return true
	}
	switch compact {
	case "[图片]", "[视频]", "[语音]", "[动画表情]", "[语音通话]", "[ok]", "ok", "好", "好的", "好了", "收到", "收到谢谢", "嗯", "嗯嗯", "对", "是的", "可以", "谢谢", "👌":
		return true
	}
	if strings.HasPrefix(content, "[文件|") && strings.HasSuffix(content, "]") {
		return true
	}
	if strings.HasPrefix(content, "@") {
		withoutMentions := mentionTextRe.ReplaceAllString(content, "")
		withoutMentions = strings.TrimSpace(strings.ReplaceAll(withoutMentions, "\u2005", ""))
		if utf8.RuneCountInString(withoutMentions) < 4 {
			return true
		}
	}
	if utf8.RuneCountInString(content) <= 2 {
		return true
	}
	return false
}

func shouldSkipGraphSource(rec SourceRecord) bool {
	switch rec.SourceType {
	case "message":
		var msgType int64
		if rec.Metadata != nil {
			_, _ = fmt.Sscanf(rec.Metadata["msg_type"], "%d", &msgType)
		}
		return shouldSkipGraphMessageContent(rec.Content, msgType)
	case "message_chunk":
		content := strings.TrimSpace(rec.Content)
		if utf8.RuneCountInString(content) < 20 {
			return true
		}
		lines := strings.Split(content, "\n")
		useful := 0
		for _, line := range lines {
			text := line
			if idx := strings.Index(text, ":"); idx >= 0 {
				text = text[idx+1:]
			}
			if !shouldSkipGraphMessageContent(text, 1) {
				useful++
			}
			if useful >= 2 {
				return false
			}
		}
		return true
	default:
		return strings.TrimSpace(rec.Content) == ""
	}
}

func participantsForMessages(msgs []*model.Message) []GraphParticipant {
	seen := map[string]struct{}{}
	out := []GraphParticipant{}
	add := func(username, display, kind string) {
		username = strings.TrimSpace(username)
		display = strings.TrimSpace(display)
		if username == "" && display == "" {
			return
		}
		key := kind + ":" + username + ":" + display
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, GraphParticipant{UserName: username, DisplayName: display, Kind: kind})
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		add(m.Talker, m.TalkerName, "conversation")
		add(m.Sender, m.SenderName, "person")
		if len(out) >= 80 {
			break
		}
	}
	return out
}

func contextForMessage(target *model.Message, scope []*model.Message) []ContextMessage {
	if target == nil || len(scope) == 0 {
		return nil
	}
	idx := -1
	for i, msg := range scope {
		if msg != nil && msg.Seq == target.Seq {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	start := idx - contextBeforeCount
	if start < 0 {
		start = 0
	}
	end := idx + contextAfterCount + 1
	if end > len(scope) {
		end = len(scope)
	}
	out := make([]ContextMessage, 0, end-start)
	for i := start; i < end; i++ {
		msg := scope[i]
		if msg == nil {
			continue
		}
		role := "context"
		if msg.Seq == target.Seq {
			role = "target"
		} else if msg.Time.Before(target.Time) {
			role = "before"
		} else {
			role = "after"
		}
		text := messageGraphText(msg)
		if text == "" {
			continue
		}
		sender := strings.TrimSpace(msg.SenderName)
		if sender == "" {
			sender = msg.Sender
		}
		out = append(out, ContextMessage{
			Seq: msg.Seq, Time: msg.Time.Format("2006-01-02 15:04:05"), Sender: sender,
			Content: truncateRunes(text, 600), Role: role,
		})
	}
	return out
}

func (m *Manager) enqueueSessionChunks(talker string, msgs []*model.Message, participants []GraphParticipant) {
	if len(msgs) == 0 {
		return
	}
	chunk := make([]*model.Message, 0, chunkMessageCount)
	flush := func() {
		if len(chunk) == 0 {
			return
		}
		rec := sessionChunkSource(talker, chunk, participants)
		_, _, _ = m.store.UpsertSource(rec)
		chunk = chunk[:0]
	}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		if shouldSkipGraphMessageContent(messageGraphText(msg), msg.Type) {
			continue
		}
		if len(chunk) > 0 && (len(chunk) >= chunkMessageCount || msg.Time.Sub(chunk[len(chunk)-1].Time) > chunkMaxGap) {
			flush()
		}
		chunk = append(chunk, msg)
	}
	flush()
}

func sessionChunkSource(talker string, msgs []*model.Message, participants []GraphParticipant) SourceRecord {
	first := msgs[0]
	last := msgs[len(msgs)-1]
	lines := make([]string, 0, len(msgs))
	context := make([]ContextMessage, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		text := messageGraphText(msg)
		if text == "" {
			continue
		}
		sender := strings.TrimSpace(msg.SenderName)
		if sender == "" {
			sender = msg.Sender
		}
		line := fmt.Sprintf("%s %s: %s", msg.Time.Format("15:04:05"), sender, truncateRunes(text, 800))
		lines = append(lines, line)
		context = append(context, ContextMessage{Seq: msg.Seq, Time: msg.Time.Format("2006-01-02 15:04:05"), Sender: sender, Content: truncateRunes(text, 800), Role: "chunk"})
	}
	return SourceRecord{
		SourceID:   fmt.Sprintf("%s:%d:%d", talker, first.Seq, last.Seq),
		SourceType: "message_chunk",
		EventType:  "chat_context",
		Talker:     first.Talker, TalkerName: first.TalkerName, Sender: "", SenderName: "",
		Title:   fmt.Sprintf("%s %s-%s 会话片段", first.TalkerName, first.Time.Format("2006-01-02 15:04"), last.Time.Format("15:04")),
		Content: strings.Join(lines, "\n"), EventTime: first.Time,
		Metadata: map[string]string{
			"start_seq": fmt.Sprintf("%d", first.Seq), "end_seq": fmt.Sprintf("%d", last.Seq),
			"message_count": fmt.Sprintf("%d", len(msgs)),
		},
		Context: context, Participants: participants,
	}
}

func (m *Manager) Query(keyword, entity, relation string, start, end time.Time, limit int) (QueryResult, error) {
	return m.store.Query(keyword, entity, relation, start, end, limit)
}

func (m *Manager) Timeline(keyword string, start, end time.Time, limit int) ([]GraphTimeline, error) {
	v, err := m.store.Visualize(keyword, start, end, limit)
	return v.Timeline, err
}

func (m *Manager) Visualize(keyword string, start, end time.Time, limit int) (VisualizeResult, error) {
	return m.store.Visualize(keyword, start, end, limit)
}

func (m *Manager) QA(ctx context.Context, query string, start, end time.Time) (string, QueryResult, error) {
	result, err := m.Query(query, "", "", start, end, 30)
	if err != nil {
		return "", result, err
	}
	cfgPtr := m.conf.GetSemanticConfig()
	if cfgPtr == nil || !conf.SemanticChatReady(*cfgPtr) {
		return graphLocalAnswer(query, result), result, nil
	}
	cfg := conf.NormalizeSemanticConfig(*cfgPtr)
	prompt := "请基于下面的时间知识图谱证据回答用户问题。要求：说明事实、关系如何随时间变化；无法确定时明确说证据不足；引用关键证据。\n\n问题：" + query + "\n\n证据：" + toJSONString(result)
	answer, err := m.client.Chat(ctx, cfg, []semantic.ChatMessage{{Role: "user", Content: prompt}})
	if err != nil {
		return graphLocalAnswer(query, result), result, nil
	}
	return answer, result, nil
}

func graphLocalAnswer(query string, result QueryResult) string {
	parts := []string{fmt.Sprintf("图谱检索到 %d 个实体、%d 条关系、%d 个事件、%d 条事实。", len(result.Entities), len(result.Relations), len(result.Events), len(result.Facts))}
	for i, rel := range result.Relations {
		if i >= 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("- 关系：%s --%s--> %s（状态 %s）", rel.Subject, rel.Predicate, rel.Object, rel.Status))
	}
	for i, ev := range result.Events {
		if i >= 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("- 事件：%s %s", time.Unix(ev.EventTime, 0).Format("2006-01-02"), ev.Title))
	}
	return strings.Join(parts, "\n")
}
