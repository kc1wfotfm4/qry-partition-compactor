package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

func TestHealthzReportsOK(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	NewRouter(st).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestUnknownRouteUsesPublishedErrorShape(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	NewRouter(st).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestPartitionReportAndUpdate(t *testing.T) {
	router := newTestRouter(t)

	recorder := doJSON(t, router, http.MethodPost, "/api/v1/tables/7/partitions",
		`{"partition_key":"dt","partition_value":"2024-01-01","file_count":4,"target_file_size":1024}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	snapshot := decodeBody(t, recorder)
	if snapshot["table_id"] != float64(7) || snapshot["partition_key"] != "dt" ||
		snapshot["partition_value"] != "2024-01-01" || snapshot["file_count"] != float64(4) ||
		snapshot["target_file_size"] != float64(1024) {
		t.Fatalf("snapshot = %v", snapshot)
	}

	recorder = doJSON(t, router, http.MethodPost, "/api/v1/tables/7/partitions",
		`{"partition_key":"dt","partition_value":"2024-01-01","file_count":1,"target_file_size":2048}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	recorder = doJSON(t, router, http.MethodGet, "/api/v1/tables/7/advice", "")
	advice := decodeBody(t, recorder)["advice"].([]any)
	if len(advice) != 1 {
		t.Fatalf("advice entries = %d, want 1 (update must not duplicate)", len(advice))
	}
	entry := advice[0].(map[string]any)
	if entry["file_count"] != float64(1) || entry["target_file_size"] != float64(2048) {
		t.Fatalf("updated entry = %v", entry)
	}
}

func TestPartitionValidation(t *testing.T) {
	router := newTestRouter(t)

	cases := []struct {
		name    string
		path    string
		body    string
		message string
	}{
		{"missing field", "/api/v1/tables/7/partitions", `{"partition_key":"dt","partition_value":"a","target_file_size":1024}`, "file_count"},
		{"negative file count", "/api/v1/tables/7/partitions", `{"partition_key":"dt","partition_value":"a","file_count":-1,"target_file_size":1024}`, "file_count"},
		{"zero target size", "/api/v1/tables/7/partitions", `{"partition_key":"dt","partition_value":"a","file_count":0,"target_file_size":0}`, "target_file_size"},
		{"wrong type", "/api/v1/tables/7/partitions", `{"partition_key":"dt","partition_value":"a","file_count":"many","target_file_size":1024}`, "file_count"},
		{"non-integer table id", "/api/v1/tables/abc/partitions", `{"partition_key":"dt","partition_value":"a","file_count":1,"target_file_size":1024}`, "table id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doJSON(t, router, http.MethodPost, tc.path, tc.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			payload := decodeBody(t, recorder)
			if len(payload) != 1 {
				t.Fatalf("error body keys = %v, want only error", payload)
			}
			errObj, ok := payload["error"].(map[string]any)
			if !ok || len(errObj) != 2 {
				t.Fatalf("error object = %v, want code and message only", payload["error"])
			}
			if errObj["code"] != "invalid_input" {
				t.Fatalf("code = %v, want invalid_input", errObj["code"])
			}
			message, ok := errObj["message"].(string)
			if !ok || !strings.Contains(message, tc.message) {
				t.Fatalf("message = %v, want it to mention %q", errObj["message"], tc.message)
			}
		})
	}
}

func TestAdviceOrderingAndActions(t *testing.T) {
	router := newTestRouter(t)

	recorder := doJSON(t, router, http.MethodGet, "/api/v1/tables/42/advice", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if got := decodeBody(t, recorder)["error"].(map[string]any)["code"]; got != "table_not_found" {
		t.Fatalf("code = %v, want table_not_found", got)
	}

	reports := []string{
		`{"partition_key":"dt","partition_value":"b","file_count":1,"target_file_size":512}`,
		`{"partition_key":"dt","partition_value":"c","file_count":5,"target_file_size":512}`,
		`{"partition_key":"dt","partition_value":"a","file_count":0,"target_file_size":512}`,
	}
	for _, body := range reports {
		if rec := doJSON(t, router, http.MethodPost, "/api/v1/tables/42/partitions", body); rec.Code != http.StatusOK {
			t.Fatalf("report status = %d, want %d", rec.Code, http.StatusOK)
		}
	}

	recorder = doJSON(t, router, http.MethodGet, "/api/v1/tables/42/advice", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	advice := decodeBody(t, recorder)["advice"].([]any)
	want := []struct {
		value  string
		action string
		reason string
	}{
		{"a", "no_compaction", "empty_partition"},
		{"b", "no_compaction", "already_compact"},
		{"c", "compact", "multiple_files"},
	}
	if len(advice) != len(want) {
		t.Fatalf("advice entries = %d, want %d", len(advice), len(want))
	}
	for i, expected := range want {
		entry := advice[i].(map[string]any)
		if entry["partition_value"] != expected.value || entry["action"] != expected.action || entry["reason"] != expected.reason {
			t.Fatalf("advice[%d] = %v, want value=%s action=%s reason=%s", i, entry, expected.value, expected.action, expected.reason)
		}
		if entry["target_file_size"] != float64(512) {
			t.Fatalf("advice[%d] target_file_size = %v, want 512", i, entry["target_file_size"])
		}
	}
}

func TestCompactionLifecycle(t *testing.T) {
	router := newTestRouter(t)

	recorder := doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":9,"partition_value":"a"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if got := decodeBody(t, recorder)["error"].(map[string]any)["code"]; got != "table_not_found" {
		t.Fatalf("code = %v, want table_not_found", got)
	}

	doJSON(t, router, http.MethodPost, "/api/v1/tables/9/partitions",
		`{"partition_key":"dt","partition_value":"a","file_count":3,"target_file_size":256}`)

	recorder = doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":9}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := decodeBody(t, recorder)["error"].(map[string]any)["code"]; got != "invalid_input" {
		t.Fatalf("code = %v, want invalid_input", got)
	}

	recorder = doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":"nine","partition_value":"a"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	recorder = doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":9,"partition_value":"missing"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if got := decodeBody(t, recorder)["error"].(map[string]any)["code"]; got != "partition_not_found" {
		t.Fatalf("code = %v, want partition_not_found", got)
	}

	recorder = doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":9,"partition_value":"a"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	created := decodeBody(t, recorder)
	if created["status"] != "running" {
		t.Fatalf("status = %v, want running", created["status"])
	}
	if created["table_id"] != float64(9) || created["partition_key"] != "dt" ||
		created["partition_value"] != "a" || created["file_count"] != float64(3) ||
		created["target_file_size"] != float64(256) {
		t.Fatalf("snapshot fields = %v", created)
	}
	id, ok := created["id"].(float64)
	if !ok || id < 1 {
		t.Fatalf("id = %v, want a positive number", created["id"])
	}

	recorder = doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":9,"partition_value":"a"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if got := decodeBody(t, recorder)["error"].(map[string]any)["code"]; got != "compaction_in_progress" {
		t.Fatalf("code = %v, want compaction_in_progress", got)
	}
}

func TestGetCompaction(t *testing.T) {
	router := newTestRouter(t)

	doJSON(t, router, http.MethodPost, "/api/v1/tables/5/partitions",
		`{"partition_key":"dt","partition_value":"x","file_count":2,"target_file_size":128}`)
	created := decodeBody(t, doJSON(t, router, http.MethodPost, "/api/v1/compactions", `{"table_id":5,"partition_value":"x"}`))
	id := created["id"].(float64)

	recorder := doJSON(t, router, http.MethodGet, "/api/v1/compactions/"+strconv.FormatInt(int64(id), 10), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	loaded := decodeBody(t, recorder)
	if loaded["status"] != "running" || loaded["id"] != id || loaded["partition_value"] != "x" {
		t.Fatalf("loaded = %v", loaded)
	}

	recorder = doJSON(t, router, http.MethodGet, "/api/v1/compactions/99999", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if got := decodeBody(t, recorder)["error"].(map[string]any)["code"]; got != "compaction_not_found" {
		t.Fatalf("code = %v, want compaction_not_found", got)
	}
}

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

func doJSON(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}
