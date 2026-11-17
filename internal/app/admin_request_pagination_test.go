package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestAdminRequestPaginationBounds(t *testing.T) {
	for i := 0; i < 3; i++ {
		insertRequest(&RequestRow{TS: 1, Model: "pagination-bounds", Status: 200})
	}
	t.Cleanup(func() { _, _ = getDB().Exec("DELETE FROM requests WHERE model=?", "pagination-bounds") })
	for _, tc := range []struct {
		query                string
		limit, offset, items int
	}{
		{"limit=-1&offset=-9", 1, 0, 1}, {"limit=0", 1, 0, 1}, {"limit=999", 500, 0, 3}, {"limit=2&offset=1", 2, 1, 2}, {"limit=bad&offset=bad", 50, 0, 3},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleAdminRequests(rec, httptest.NewRequest("GET", "/admin/api/requests?model=pagination-bounds&"+tc.query, nil))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Limit  int               `json:"limit"`
				Offset int               `json:"offset"`
				Items  []json.RawMessage `json:"items"`
				Total  int               `json:"total"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Limit != tc.limit || body.Offset != tc.offset || len(body.Items) != tc.items || body.Total != 3 {
				t.Fatalf("unexpected result: %+v", body)
			}
		})
	}
}
