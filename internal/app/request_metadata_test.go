package app

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestMetadataMigrationPreservesUnknownHistory(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, model TEXT); INSERT INTO requests VALUES (1, 'legacy-pro')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateRequestMetadata(conn); err != nil {
			t.Fatal(err)
		}
	}
	var requested, actual sql.NullString
	var verified, retries int
	if err := conn.QueryRow(`SELECT requested_model,actual_model,actual_model_verified,retry_count FROM requests WHERE id=1`).Scan(&requested, &actual, &verified, &retries); err != nil {
		t.Fatal(err)
	}
	if requested.Valid || actual.Valid || verified != 0 || retries != 0 {
		t.Fatalf("migration invented evidence: %v %v %d %d", requested, actual, verified, retries)
	}
}

func TestRequestMetadataAdminRoundTrip(t *testing.T) {
	row := &RequestRow{TS: 1, Model: "metadata-test-alias", RequestedModel: "metadata-test-alias", CanonicalModel: "gemini-pro", ActualModel: "gemini-flash", ActualModelVerified: true, RetryCount: 2, FailureClass: "model_mismatch", AccountLabel: "test-account", Status: 502}
	insertRequest(row)
	t.Cleanup(func() { _, _ = getDB().Exec("DELETE FROM requests WHERE model=?", row.Model) })
	rec := httptest.NewRecorder()
	handleAdminRequests(rec, httptest.NewRequest("GET", "/admin/api/requests?model=metadata-test-alias", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			RequestedModel      string `json:"requested_model"`
			CanonicalModel      string `json:"canonical_model"`
			ActualModel         string `json:"actual_model"`
			ActualModelVerified bool   `json:"actual_model_verified"`
			RetryCount          int    `json:"retry_count"`
			FailureClass        string `json:"failure_class"`
			AccountLabel        string `json:"account_label"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items: %s", rec.Body.String())
	}
	got := body.Items[0]
	if got.RequestedModel != row.RequestedModel || got.CanonicalModel != row.CanonicalModel || got.ActualModel != row.ActualModel || !got.ActualModelVerified || got.RetryCount != 2 || got.FailureClass != row.FailureClass || got.AccountLabel != row.AccountLabel {
		t.Fatalf("metadata lost: %+v", got)
	}
}

func TestRequestMetadataSchemaAllDialects(t *testing.T) {
	// Real MySQL/Postgres servers are not required for this offline DDL contract.
	for _, dialect := range []dbDialect{dialectSQLite, dialectMySQL, dialectPostgres} {
		found := false
		for _, stmt := range schemaStatements(dialect) {
			if strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS requests") {
				found = true
				for _, name := range []string{"requested_model", "canonical_model", "actual_model", "actual_model_verified", "retry_count", "failure_class"} {
					if !strings.Contains(stmt, name) {
						t.Fatalf("dialect %d missing %s", dialect, name)
					}
				}
			}
		}
		if !found {
			t.Fatalf("dialect %d missing requests", dialect)
		}
	}
}
