package controlplane

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shift.dev/shift/internal/database"
)

func TestWriteInternalErrorHidesCause(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{logger: logger}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/organizations/x/machines", nil)
	request.Header.Set("X-Request-ID", "req-123")

	leaky := errors.New(`ERROR: duplicate key value violates unique constraint "sessions_pkey" (SQLSTATE 23505)`)
	server.writeInternalError(recorder, request, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", leaky)

	body := recorder.Body.String()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if strings.Contains(body, "duplicate key") || strings.Contains(body, "23505") || strings.Contains(body, "sessions_pkey") {
		t.Fatalf("5xx body leaked the error cause: %s", body)
	}
	if !strings.Contains(body, "MACHINES_LOOKUP_FAILED") {
		t.Fatalf("5xx body lost its stable error code: %s", body)
	}
	if !strings.Contains(body, "req-123") {
		t.Fatalf("5xx body should carry the request id for correlation: %s", body)
	}
}

func TestPageFromRequestClampsBounds(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/organizations/x/machines?limit=99999&offset=-4", nil)
	page := pageFromRequest(request)
	if page.Limit != database.MaxPageLimit {
		t.Fatalf("limit = %d, want clamped to %d", page.Limit, database.MaxPageLimit)
	}
	if page.Offset != 0 {
		t.Fatalf("offset = %d, want clamped to 0", page.Offset)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/organizations/x/machines", nil)
	if page = pageFromRequest(request); page.Limit != database.DefaultPageLimit {
		t.Fatalf("default limit = %d, want %d", page.Limit, database.DefaultPageLimit)
	}
}
