package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sjzar/chatlog/internal/chatlog/temporalgraph"
	"github.com/sjzar/chatlog/internal/errors"
)

func (s *Service) requireGraph(c *gin.Context) *temporalgraph.Manager {
	if s.graph == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "temporal graph manager unavailable"})
		return nil
	}
	return s.graph
}

func (s *Service) handleGraphStatus(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	writeByFormat(c, g.Status(), c.Query("format"))
}

func (s *Service) handleGraphConfigGet(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	st := g.Status()
	writeByFormat(c, gin.H{
		"workers":         st.Workers,
		"enqueue_workers": st.EnqueueWorkers,
	}, c.Query("format"))
}

func (s *Service) handleGraphConfigSet(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	var req struct {
		Workers        int `json:"workers"`
		EnqueueWorkers int `json:"enqueue_workers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		errors.Err(c, errors.InvalidArg("body"))
		return
	}
	if req.Workers <= 0 {
		req.Workers = 1
	}
	if req.EnqueueWorkers <= 0 {
		req.EnqueueWorkers = 1
	}
	if err := g.SetWorkers(req.Workers, req.EnqueueWorkers); err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, gin.H{"ok": true, "status": g.Status()}, c.Query("format"))
}

func (s *Service) handleGraphIngestMessage(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	var batch []temporalgraph.IngestMessage
	if err := bindSingleOrBatch(c, &batch); err != nil {
		errors.Err(c, err)
		return
	}
	ids := make([]int64, 0, len(batch))
	for _, item := range batch {
		id, err := g.IngestMessage(c.Request.Context(), item)
		if err != nil {
			errors.Err(c, err)
			return
		}
		ids = append(ids, id)
	}
	writeByFormat(c, gin.H{"ok": true, "count": len(ids), "ids": ids, "status": g.Status()}, c.Query("format"))
}

func (s *Service) handleGraphIngestBusiness(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	var batch []temporalgraph.IngestBusiness
	if err := bindSingleOrBatch(c, &batch); err != nil {
		errors.Err(c, err)
		return
	}
	ids := make([]int64, 0, len(batch))
	for _, item := range batch {
		id, err := g.IngestBusiness(c.Request.Context(), item)
		if err != nil {
			errors.Err(c, err)
			return
		}
		ids = append(ids, id)
	}
	writeByFormat(c, gin.H{"ok": true, "count": len(ids), "ids": ids, "status": g.Status()}, c.Query("format"))
}

func (s *Service) handleGraphIngestEvent(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	var batch []temporalgraph.IngestEvent
	if err := bindSingleOrBatch(c, &batch); err != nil {
		errors.Err(c, err)
		return
	}
	ids := make([]int64, 0, len(batch))
	for _, item := range batch {
		id, err := g.IngestEvent(c.Request.Context(), item)
		if err != nil {
			errors.Err(c, err)
			return
		}
		ids = append(ids, id)
	}
	writeByFormat(c, gin.H{"ok": true, "count": len(ids), "ids": ids, "status": g.Status()}, c.Query("format"))
}

func bindSingleOrBatch[T any](c *gin.Context, out *[]T) error {
	raw, err := c.GetRawData()
	if err != nil {
		return err
	}
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return fmt.Errorf("empty request body")
	}
	if raw[0] == '[' {
		return json.Unmarshal(raw, out)
	}
	var item T
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	*out = []T{item}
	return nil
}

func (s *Service) handleGraphRebuild(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	var req struct {
		Reset bool `json:"reset"`
	}
	_ = c.ShouldBindJSON(&req)
	if c.Query("reset") == "1" || c.Query("reset") == "true" {
		req.Reset = true
	}
	if err := g.Rebuild(context.Background(), req.Reset); err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, gin.H{"ok": true, "accepted": true, "status": g.Status()}, c.Query("format"))
}

func (s *Service) handleGraphPause(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	if err := g.Pause(); err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, gin.H{"ok": true, "status": g.Status()}, c.Query("format"))
}

func (s *Service) handleGraphResume(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	if err := g.Resume(); err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, gin.H{"ok": true, "status": g.Status()}, c.Query("format"))
}

func (s *Service) handleGraphQuery(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	start, end := graphTimeRange(c)
	limit := graphLimit(c)
	result, err := g.Query(c.Query("keyword"), c.Query("entity"), c.Query("relation"), start, end, limit)
	if err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, result, c.Query("format"))
}

func (s *Service) handleGraphTimeline(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	start, end := graphTimeRange(c)
	items, err := g.Timeline(c.Query("keyword"), start, end, graphLimit(c))
	if err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, gin.H{"items": items, "count": len(items)}, c.Query("format"))
}

func (s *Service) handleGraphVisualize(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	start, end := graphTimeRange(c)
	result, err := g.Visualize(c.Query("keyword"), start, end, graphLimit(c))
	if err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, result, c.Query("format"))
}

func (s *Service) handleGraphQA(c *gin.Context) {
	g := s.requireGraph(c)
	if g == nil {
		return
	}
	var req struct {
		Query  string `json:"query"`
		Window string `json:"window"`
		Start  string `json:"start"`
		End    string `json:"end"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		errors.Err(c, err)
		return
	}
	start, end := graphWindow(req.Window, req.Start, req.End)
	answer, evidence, err := g.QA(c.Request.Context(), strings.TrimSpace(req.Query), start, end)
	if err != nil {
		errors.Err(c, err)
		return
	}
	writeByFormat(c, gin.H{"answer": answer, "evidence": evidence}, c.Query("format"))
}

func graphLimit(c *gin.Context) int {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "80"))
	if limit <= 0 {
		limit = 80
	}
	if limit > 300 {
		limit = 300
	}
	return limit
}

func graphTimeRange(c *gin.Context) (time.Time, time.Time) {
	return graphWindow(c.DefaultQuery("window", ""), c.Query("start"), c.Query("end"))
}

func graphWindow(window, startRaw, endRaw string) (time.Time, time.Time) {
	now := time.Now()
	var start, end time.Time
	if strings.TrimSpace(startRaw) != "" {
		start = parseGraphTime(startRaw)
	}
	if strings.TrimSpace(endRaw) != "" {
		end = parseGraphTime(endRaw)
	}
	if !start.IsZero() || !end.IsZero() {
		return start, end
	}
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "today", "1d":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), now
	case "7d":
		return now.AddDate(0, 0, -7), now
	case "30d":
		return now.AddDate(0, -1, 0), now
	case "90d":
		return now.AddDate(0, -3, 0), now
	case "1y":
		return now.AddDate(-1, 0, 0), now
	default:
		return time.Time{}, time.Time{}
	}
}

func parseGraphTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}
