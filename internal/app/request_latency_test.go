package app

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

var latencyColumns = []string{"catalog_ms", "upstream_first_text_ms", "model_verified_ms", "downstream_first_text_ms", "first_tool_ms", "first_reasoning_ms", "generation_start_ms", "first_heartbeat_ms"}

func TestRequestLatencyMigrationUnknown(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE requests(id INTEGER PRIMARY KEY); INSERT INTO requests VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = migrateRequestMetadata(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range append(append([]string{}, latencyColumns...), "prompt_bytes", "prompt_budget_bytes", "context_attachment_used", "prepare_ms") {
		var v sql.NullInt64
		if err = db.QueryRow("SELECT " + col + " FROM requests WHERE id=1").Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v.Valid {
			t.Fatalf("%s invented historical timing", col)
		}
	}
	for _, d := range []dbDialect{dialectSQLite, dialectMySQL, dialectPostgres} {
		ddl := strings.Join(schemaStatements(d), ";")
		for _, col := range latencyColumns {
			if !strings.Contains(ddl, col) {
				t.Fatalf("dialect %d missing %s", d, col)
			}
		}
	}
}

func TestRequestLatencyAdminNullAndZero(t *testing.T) {
	zero, one, two, three, four, five, six := int64(0), int64(1), int64(2), int64(3), int64(4), int64(5), int64(6)
	row := &RequestRow{TS: 1, Model: "latency-roundtrip", Status: 200, CatalogMs: &zero, UpstreamFirstTextMs: &one, ModelVerifiedMs: &two, DownstreamFirstTextMs: &three, FirstToolMs: &four, FirstReasoningMs: &five, GenerationStartMs: &six}
	insertRequest(row)
	t.Cleanup(func() { _, _ = getDB().Exec("DELETE FROM requests WHERE model=?", row.Model) })
	rec := httptest.NewRecorder()
	handleAdminRequests(rec, httptest.NewRequest("GET", "/admin/api/requests?model=latency-roundtrip", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("rows %s", rec.Body.String())
	}
	for i, col := range latencyColumns {
		got, exists := body.Items[0][col]
		if !exists {
			t.Fatalf("missing %s", col)
		}
		if i == 7 {
			if got != nil {
				t.Fatalf("unobserved heartbeat: %v", got)
			}
		} else if got != float64(i) {
			t.Fatalf("%s = %v, want %d", col, got, i)
		}
	}
}

func TestRequestPreparationNullableRoundTrip(t *testing.T) {
	zero, budget, prepare := int64(0), int64(12345), int64(20)
	no, yes := false, true
	for _, used := range []*bool{nil, &no, &yes} {
		row := &RequestRow{TS: 1, Model: "prepare-roundtrip", Status: 200, ContextAttachmentUsed: used}
		if used != nil {
			row.PromptBytes = &zero
			row.PromptBudgetBytes = &budget
			row.PrepareMs = &prepare
		}
		insertRequest(row)
		rec := httptest.NewRecorder()
		handleAdminRequests(rec, httptest.NewRequest("GET", "/admin/api/requests?model=prepare-roundtrip&limit=1", nil))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Items) != 1 {
			t.Fatalf("rows: %s", rec.Body.String())
		}
		item := body.Items[0]
		for _, col := range []string{"prompt_bytes", "prompt_budget_bytes", "context_attachment_used", "prepare_ms"} {
			if _, ok := item[col]; !ok {
				t.Fatalf("missing %s", col)
			}
			if used == nil && item[col] != nil {
				t.Fatalf("%s must be null: %v", col, item[col])
			}
		}
		if used != nil && (item["context_attachment_used"] != *used || item["prompt_bytes"] != float64(0) || item["prompt_budget_bytes"] != float64(budget) || item["prepare_ms"] != float64(prepare)) {
			t.Fatalf("values: %v", item)
		}
	}
	t.Cleanup(func() { _, _ = getDB().Exec("DELETE FROM requests WHERE model=?", "prepare-roundtrip") })
}
