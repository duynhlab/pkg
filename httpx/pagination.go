package httpx

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// Pagination query defaults and bounds.
const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ParsePage reads the "page" and "page_size" query params, applying defaults and
// the MaxPageSize cap. Invalid or out-of-range values fall back to defaults rather
// than erroring, so list endpoints stay forgiving for callers.
func ParsePage(c *gin.Context) (page, pageSize int) {
	page = atoiDefault(c.Query("page"), DefaultPage)
	if page < 1 {
		page = DefaultPage
	}
	pageSize = atoiDefault(c.Query("page_size"), DefaultPageSize)
	if pageSize < 1 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize
}

// Offset returns the SQL OFFSET for the given page/size (page is 1-based).
func Offset(page, pageSize int) int {
	return (page - 1) * pageSize
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Paginated is the standard list-response envelope.
type Paginated[T any] struct {
	Items      []T `json:"items"`
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	TotalItems int `json:"total_items"`
	TotalPages int `json:"total_pages"`
}

// NewPaginated builds the envelope, computing total_pages and guaranteeing a
// non-nil Items slice so JSON renders "items": [] rather than "items": null.
func NewPaginated[T any](items []T, page, pageSize, totalItems int) Paginated[T] {
	if items == nil {
		items = []T{}
	}
	totalPages := 0
	if pageSize > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}
	return Paginated[T]{
		Items:      items,
		Page:       page,
		PageSize:   pageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}
