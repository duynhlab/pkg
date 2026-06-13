package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParsePage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		query    string
		wantPage int
		wantSize int
	}{
		{"defaults when empty", "", DefaultPage, DefaultPageSize},
		{"explicit values", "?page=3&page_size=10", 3, 10},
		{"non-numeric falls back", "?page=abc&page_size=xyz", DefaultPage, DefaultPageSize},
		{"zero and negative fall back", "?page=0&page_size=-5", DefaultPage, DefaultPageSize},
		{"size capped at max", "?page=2&page_size=1000", 2, MaxPageSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/"+tt.query, nil)
			page, size := ParsePage(c)
			if page != tt.wantPage || size != tt.wantSize {
				t.Fatalf("ParsePage(%q) = (%d,%d), want (%d,%d)", tt.query, page, size, tt.wantPage, tt.wantSize)
			}
		})
	}
}

func TestOffset(t *testing.T) {
	if got := Offset(1, 20); got != 0 {
		t.Fatalf("Offset(1,20) = %d, want 0", got)
	}
	if got := Offset(3, 20); got != 40 {
		t.Fatalf("Offset(3,20) = %d, want 40", got)
	}
}

func TestNewPaginated(t *testing.T) {
	tests := []struct {
		name           string
		items          []int
		page, size     int
		total          int
		wantTotalPages int
		wantLen        int
	}{
		{"exact multiple", []int{1, 2}, 1, 20, 40, 2, 2},
		{"rounds up", []int{1}, 1, 20, 41, 3, 1},
		{"empty stays non-nil", nil, 1, 20, 0, 0, 0},
		{"zero size no divide", []int{1}, 1, 0, 5, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPaginated(tt.items, tt.page, tt.size, tt.total)
			if p.TotalPages != tt.wantTotalPages {
				t.Errorf("TotalPages = %d, want %d", p.TotalPages, tt.wantTotalPages)
			}
			if p.Items == nil {
				t.Error("Items must never be nil")
			}
			if len(p.Items) != tt.wantLen {
				t.Errorf("len(Items) = %d, want %d", len(p.Items), tt.wantLen)
			}
		})
	}
}

func TestNewPaginatedEmptyMarshalsToArray(t *testing.T) {
	p := NewPaginated[int](nil, 1, 20, 0)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["items"]) != "[]" {
		t.Fatalf("items = %s, want []", raw["items"])
	}
}

func TestRespondError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	RespondError(c, http.StatusNotFound, CodeNotFound, "Product not found")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "Product not found" {
		t.Errorf("error = %q, want %q", body.Error, "Product not found")
	}
	if body.Code != CodeNotFound {
		t.Errorf("code = %q, want %q", body.Code, CodeNotFound)
	}
}
