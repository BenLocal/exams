package web

import (
	"context"

	"github.com/BenLocal/exams/internal/store"
	"github.com/cloudwego/hertz/pkg/app"
)

// The JSON API mirrors the pages. Both read their filter from the same
// filterFromQuery, so the two cannot drift apart.

func (s *Server) apiListExams(ctx context.Context, c *app.RequestContext) {
	page, err := s.store.ListExams(ctx, filterFromQuery(c))
	if err != nil {
		s.fail(c, "list exams failed", err)
		return
	}
	c.JSON(200, map[string]any{
		"items": page.Items,
		"total": page.Total,
		"page":  page.Page,
		// Note: list results carry no body text. Fetch /api/exams/:id for it.
		"per_page":    page.PerPage,
		"total_pages": page.TotalPages,
	})
}

func (s *Server) apiGetExam(ctx context.Context, c *app.RequestContext) {
	id, ok := parseID(c.Param("id"))
	if !ok {
		c.JSON(400, map[string]string{"error": "invalid exam id"})
		return
	}
	exam, err := s.store.GetExam(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			c.JSON(404, map[string]string{"error": "not found"})
			return
		}
		s.fail(c, "get exam failed", err)
		return
	}
	changes, err := s.store.ExamChanges(ctx, id, 50)
	if err != nil {
		s.fail(c, "exam changes failed", err)
		return
	}
	c.JSON(200, map[string]any{"exam": exam, "changes": changes})
}

func (s *Server) apiRuns(ctx context.Context, c *app.RequestContext) {
	runs, err := s.store.ListRuns(ctx, c.Query("source"), 100)
	if err != nil {
		s.fail(c, "list runs failed", err)
		return
	}
	c.JSON(200, map[string]any{"runs": runs})
}

func (s *Server) apiStats(ctx context.Context, c *app.RequestContext) {
	stats, err := s.store.Stats(ctx)
	if err != nil {
		s.fail(c, "stats failed", err)
		return
	}
	facets, err := s.store.Facets(ctx)
	if err != nil {
		s.fail(c, "facets failed", err)
		return
	}
	c.JSON(200, map[string]any{"stats": stats, "facets": facets})
}
