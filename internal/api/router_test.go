package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

func serveRequest(t *testing.T, router http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func assertErrorShape(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, wantStatus, recorder.Body.String())
	}
	decoded := decodeResponse(t, recorder)
	if len(decoded) != 1 {
		t.Fatalf("error body keys = %v, want only the top-level error", decoded)
	}
	errObj, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("error body = %v, want an error object", decoded)
	}
	if len(errObj) != 2 {
		t.Fatalf("error object keys = %v, want only code and message", errObj)
	}
	if errObj["code"] != wantCode {
		t.Fatalf("code = %v, want %s", errObj["code"], wantCode)
	}
	message, ok := errObj["message"].(string)
	if !ok || message == "" {
		t.Fatalf("message = %v, want a non-empty string", errObj["message"])
	}
	for _, leaked := range []string{"select ", "insert ", "update ", "sql", "goroutine", ".go:", "/"} {
		if strings.Contains(strings.ToLower(message), leaked) {
			t.Fatalf("message %q leaks %q", message, leaked)
		}
	}
	return errObj
}

func reportPartition(t *testing.T, router http.Handler, tableID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return serveRequest(t, router, http.MethodPost, "/api/v1/tables/"+tableID+"/partitions", body)
}

func TestHealthzReportsOK(t *testing.T) {
	router := newTestRouter(t)

	recorder := serveRequest(t, router, http.MethodGet, "/healthz", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestUnknownRouteUsesPublishedErrorShape(t *testing.T) {
	router := newTestRouter(t)

	recorder := serveRequest(t, router, http.MethodGet, "/missing", nil)

	assertErrorShape(t, recorder, http.StatusNotFound, "route_not_found")
}

func TestReportPartitionRejectsInvalidInput(t *testing.T) {
	router := newTestRouter(t)

	cases := []struct {
		name        string
		path        string
		body        any
		wantMention string
	}{
		{"missing partition_key", "/api/v1/tables/1/partitions", map[string]any{"partition_value": "a", "file_count": 1, "target_file_size": 1}, "partition_key"},
		{"missing partition_value", "/api/v1/tables/1/partitions", map[string]any{"partition_key": "dt", "file_count": 1, "target_file_size": 1}, "partition_value"},
		{"missing file_count", "/api/v1/tables/1/partitions", map[string]any{"partition_key": "dt", "partition_value": "a", "target_file_size": 1}, "file_count"},
		{"negative file_count", "/api/v1/tables/1/partitions", map[string]any{"partition_key": "dt", "partition_value": "a", "file_count": -1, "target_file_size": 1}, "file_count"},
		{"missing target_file_size", "/api/v1/tables/1/partitions", map[string]any{"partition_key": "dt", "partition_value": "a", "file_count": 1}, "target_file_size"},
		{"zero target_file_size", "/api/v1/tables/1/partitions", map[string]any{"partition_key": "dt", "partition_value": "a", "file_count": 1, "target_file_size": 0}, "target_file_size"},
		{"negative target_file_size", "/api/v1/tables/1/partitions", map[string]any{"partition_key": "dt", "partition_value": "a", "file_count": 1, "target_file_size": -5}, "target_file_size"},
		{"non-integer table id", "/api/v1/tables/abc/partitions", map[string]any{"partition_key": "dt", "partition_value": "a", "file_count": 1, "target_file_size": 1}, "table id"},
		{"malformed json", "/api/v1/tables/1/partitions", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var recorder *httptest.ResponseRecorder
			if tc.body == nil {
				request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{not json"))
				recorder = httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
			} else {
				recorder = serveRequest(t, router, http.MethodPost, tc.path, tc.body)
			}
			errObj := assertErrorShape(t, recorder, http.StatusBadRequest, "invalid_input")
			if tc.wantMention != "" && !strings.Contains(errObj["message"].(string), tc.wantMention) {
				t.Fatalf("message %q should mention %q", errObj["message"], tc.wantMention)
			}
		})
	}
}

func TestReportPartitionStoresAndUpdatesSnapshot(t *testing.T) {
	router := newTestRouter(t)

	body := map[string]any{"partition_key": "dt", "partition_value": "2026-10-01", "file_count": 3, "target_file_size": 128}
	recorder := reportPartition(t, router, "7", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	snapshot := decodeResponse(t, recorder)
	for key, want := range map[string]any{"table_id": 7.0, "partition_key": "dt", "partition_value": "2026-10-01", "file_count": 3.0, "target_file_size": 128.0} {
		if snapshot[key] != want {
			t.Fatalf("snapshot[%s] = %v, want %v", key, snapshot[key], want)
		}
	}

	body["file_count"] = 11
	body["target_file_size"] = 256
	recorder = reportPartition(t, router, "7", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-report status = %d, want %d", recorder.Code, http.StatusOK)
	}

	advice := decodeResponse(t, serveRequest(t, router, http.MethodGet, "/api/v1/tables/7/advice", nil))
	entries := advice["advice"].([]any)
	if len(entries) != 1 {
		t.Fatalf("len(advice) = %d, want 1 after re-report", len(entries))
	}
	entry := entries[0].(map[string]any)
	if entry["file_count"] != 11.0 || entry["target_file_size"] != 256.0 {
		t.Fatalf("entry = %v, want the updated snapshot", entry)
	}
}

func TestAdviceClassifiesAndOrdersPartitions(t *testing.T) {
	router := newTestRouter(t)

	assertErrorShape(t, serveRequest(t, router, http.MethodGet, "/api/v1/tables/9/advice", nil), http.StatusNotFound, "table_not_found")

	reports := []map[string]any{
		{"partition_key": "dt", "partition_value": "2026-10-03", "file_count": 5, "target_file_size": 128},
		{"partition_key": "dt", "partition_value": "2026-10-01", "file_count": 0, "target_file_size": 128},
		{"partition_key": "dt", "partition_value": "2026-10-02", "file_count": 1, "target_file_size": 128},
	}
	for _, body := range reports {
		if recorder := reportPartition(t, router, "9", body); recorder.Code != http.StatusOK {
			t.Fatalf("report status = %d, want %d", recorder.Code, http.StatusOK)
		}
	}

	recorder := serveRequest(t, router, http.MethodGet, "/api/v1/tables/9/advice", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	advice := decodeResponse(t, recorder)["advice"].([]any)
	if len(advice) != 3 {
		t.Fatalf("len(advice) = %d, want 3", len(advice))
	}
	want := []struct {
		value  string
		action string
		reason string
	}{
		{"2026-10-01", "no_compaction", "empty_partition"},
		{"2026-10-02", "no_compaction", "already_compact"},
		{"2026-10-03", "compact", "multiple_files"},
	}
	for i, w := range want {
		entry := advice[i].(map[string]any)
		if entry["partition_value"] != w.value {
			t.Fatalf("advice[%d].partition_value = %v, want %v (ascending order)", i, entry["partition_value"], w.value)
		}
		if entry["action"] != w.action || entry["reason"] != w.reason {
			t.Fatalf("advice[%d] = %v/%v, want %v/%v", i, entry["action"], entry["reason"], w.action, w.reason)
		}
		if entry["target_file_size"] != 128.0 {
			t.Fatalf("advice[%d].target_file_size = %v, want 128", i, entry["target_file_size"])
		}
	}
}

func TestCreateCompactionLifecycle(t *testing.T) {
	router := newTestRouter(t)

	errObj := assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"partition_value": "a"}), http.StatusBadRequest, "invalid_input")
	if !strings.Contains(errObj["message"].(string), "table_id") {
		t.Fatalf("message %q should mention table_id", errObj["message"])
	}
	assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 4}), http.StatusBadRequest, "invalid_input")
	assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": "4", "partition_value": "a"}), http.StatusBadRequest, "invalid_input")
	assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 4, "partition_value": "a"}), http.StatusNotFound, "table_not_found")

	report := map[string]any{"partition_key": "dt", "partition_value": "2026-10-01", "file_count": 6, "target_file_size": 512}
	if recorder := reportPartition(t, router, "4", report); recorder.Code != http.StatusOK {
		t.Fatalf("report status = %d, want %d", recorder.Code, http.StatusOK)
	}

	assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 4, "partition_value": "2026-10-02"}), http.StatusNotFound, "partition_not_found")

	recorder := serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 4, "partition_value": "2026-10-01"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	created := decodeResponse(t, recorder)
	if created["status"] != "running" {
		t.Fatalf("status = %v, want running", created["status"])
	}
	id, ok := created["id"].(float64)
	if !ok || id == 0 {
		t.Fatalf("id = %v, want a non-zero number", created["id"])
	}
	for key, want := range map[string]any{"table_id": 4.0, "partition_key": "dt", "partition_value": "2026-10-01", "file_count": 6.0, "target_file_size": 512.0} {
		if created[key] != want {
			t.Fatalf("created[%s] = %v, want %v", key, created[key], want)
		}
	}

	assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 4, "partition_value": "2026-10-01"}), http.StatusConflict, "compaction_in_progress")

	recorder = serveRequest(t, router, http.MethodGet, "/api/v1/compactions/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", recorder.Code, http.StatusOK)
	}
	loaded := decodeResponse(t, recorder)
	if loaded["id"] != created["id"] || loaded["status"] != "running" {
		t.Fatalf("loaded = %v, want the created running task", loaded)
	}

	assertErrorShape(t, serveRequest(t, router, http.MethodGet, "/api/v1/compactions/999", nil), http.StatusNotFound, "compaction_not_found")
}

func TestAdviceDoesNotChangeTasks(t *testing.T) {
	router := newTestRouter(t)

	report := map[string]any{"partition_key": "dt", "partition_value": "2026-10-01", "file_count": 8, "target_file_size": 64}
	if recorder := reportPartition(t, router, "2", report); recorder.Code != http.StatusOK {
		t.Fatalf("report status = %d, want %d", recorder.Code, http.StatusOK)
	}
	created := serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 2, "partition_value": "2026-10-01"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", created.Code, http.StatusCreated)
	}
	id := decodeResponse(t, created)["id"]

	if recorder := serveRequest(t, router, http.MethodGet, "/api/v1/tables/2/advice", nil); recorder.Code != http.StatusOK {
		t.Fatalf("advice status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder := serveRequest(t, router, http.MethodGet, "/api/v1/compactions/1", nil); recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", recorder.Code, http.StatusOK)
	}

	loaded := decodeResponse(t, serveRequest(t, router, http.MethodGet, "/api/v1/compactions/1", nil))
	if loaded["id"] != id || loaded["status"] != "running" {
		t.Fatalf("task = %v, want id %v still running after reads", loaded, id)
	}
	assertErrorShape(t, serveRequest(t, router, http.MethodPost, "/api/v1/compactions", map[string]any{"table_id": 2, "partition_value": "2026-10-01"}), http.StatusConflict, "compaction_in_progress")
}
