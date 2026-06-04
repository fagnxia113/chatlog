package http

import (
	"context"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/sjzar/chatlog/internal/chatlog/semantic"
	"github.com/sjzar/chatlog/internal/errors"
)

type semanticQARequest struct {
	Query          string                 `json:"query"`
	Chat           string                 `json:"chat"`
	Chats          []string               `json:"chats"`
	Window         string                 `json:"window"`
	EntityOverride string                 `json:"entity_override"`
	RetrievalDepth string                 `json:"retrieval_depth"`
	SourceLimit    int                    `json:"source_limit"`
	TopN           int                    `json:"top_n"`
	History        []semantic.ChatMessage `json:"history"`
}

func (s *Service) parseSemanticQARequest(c *gin.Context) (semanticQARequest, error) {
	var req semanticQARequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return req, errors.InvalidArg("body")
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return req, errors.InvalidArg("query")
	}
	req.TopN = semanticQATopN(req.TopN, req.RetrievalDepth)
	return req, nil
}

func (s *Service) executeSemanticQA(ctx context.Context, req semanticQARequest, onDelta func(string) error) (gin.H, error) {
	talkers, err := s.semanticTalkerScope(strings.TrimSpace(req.Chat), strings.Join(req.Chats, ","), req.SourceLimit)
	if err != nil {
		return nil, err
	}
	fallbackWindow := semanticEffectiveWindow(req.Query, req.Window)
	direct, plan, err := s.trySemanticRoutedDirect(ctx, req.Query, talkers, fallbackWindow, req.EntityOverride, req.TopN)
	if err != nil {
		return nil, err
	}
	if direct != nil {
		direct.Debug = attachEmptyReason(direct.Debug, direct.Reason)
		if onDelta != nil {
			if err := onDelta(direct.Answer); err != nil {
				return nil, err
			}
		}
		return gin.H{
			"query":          req.Query,
			"chat":           req.Chat,
			"source_count":   len(talkers),
			"window":         direct.Window,
			"depth":          normalizeSemanticDepth(req.RetrievalDepth),
			"count":          direct.Count,
			"answer":         direct.Answer,
			"evidence":       direct.Evidence,
			"direct":         true,
			"debug":          direct.Debug,
			"reason":         direct.Reason,
			"rerank_tried":   false,
			"rerank_applied": false,
			"rerank_error":   "",
		}, nil
	}
	if s.semantic == nil {
		return nil, fmt.Errorf("semantic manager unavailable")
	}
	if err := s.ensureSemanticIndexReady(); err != nil {
		return nil, err
	}
	vectorQuery := semanticVectorQuery(req.Query, plan)
	vectorWindow := semanticPlanWindow(plan, fallbackWindow)
	_, _, start, end := parseSemanticWindow(vectorWindow)
	debug := semanticPlanDebug(plan, talkers, "vector/rag")
	reason := ""
	if onDelta != nil {
		var full strings.Builder
		hits, searchMeta, err := s.semantic.AnswerStreamScoped(ctx, vectorQuery, talkers, start, end, req.TopN, req.History, func(delta string) error {
			full.WriteString(delta)
			return onDelta(delta)
		})
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			reason = "向量索引在当前数据源和时间窗内无召回结果"
			debug = attachEmptyReason(debug, reason)
		}
		return semanticQAPayload(req, talkers, vectorWindow, full.String(), hits, debug, reason, searchMeta), nil
	}
	answer, hits, searchMeta, err := s.semantic.AnswerScoped(ctx, vectorQuery, talkers, start, end, req.TopN, req.History)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		reason = "向量索引在当前数据源和时间窗内无召回结果"
		debug = attachEmptyReason(debug, reason)
	}
	return semanticQAPayload(req, talkers, vectorWindow, answer, hits, debug, reason, searchMeta), nil
}

func semanticQAPayload(req semanticQARequest, talkers []string, window string, answer string, hits []semantic.SearchHit, debug gin.H, reason string, meta semantic.SearchResult) gin.H {
	return gin.H{
		"query":          req.Query,
		"chat":           req.Chat,
		"source_count":   len(talkers),
		"window":         window,
		"depth":          normalizeSemanticDepth(req.RetrievalDepth),
		"count":          len(hits),
		"answer":         answer,
		"evidence":       hits,
		"debug":          debug,
		"reason":         reason,
		"rerank_tried":   meta.RerankTried,
		"rerank_applied": meta.RerankApplied,
		"rerank_error":   meta.RerankError,
	}
}
