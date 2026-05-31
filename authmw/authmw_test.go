package authmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	authv1 "github.com/duynhlab/pkg/proto/auth/v1"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeValidator struct {
	resp   *authv1.GetMeResponse
	err    error
	called bool
}

func (f *fakeValidator) GetMe(_ context.Context, _ *authv1.GetMeRequest, _ ...grpc.CallOption) (*authv1.GetMeResponse, error) {
	f.called = true
	return f.resp, f.err
}

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	okResp := &authv1.GetMeResponse{User: &authv1.User{Id: "7", Username: "bob", Email: "bob@example.com"}}

	tests := []struct {
		name       string
		authHeader string
		val        *fakeValidator
		wantStatus int
		wantUserID string
		wantCalled bool
	}{
		{name: "missing header fails closed without calling auth", authHeader: "", val: &fakeValidator{}, wantStatus: http.StatusUnauthorized, wantCalled: false},
		{name: "unauthenticated maps to 401", authHeader: "Bearer bad", val: &fakeValidator{err: status.Error(codes.Unauthenticated, "nope")}, wantStatus: http.StatusUnauthorized, wantCalled: true},
		{name: "auth unreachable maps to 503", authHeader: "Bearer x", val: &fakeValidator{err: status.Error(codes.Unavailable, "down")}, wantStatus: http.StatusServiceUnavailable, wantCalled: true},
		{name: "internal error maps to 503", authHeader: "Bearer x", val: &fakeValidator{err: status.Error(codes.Internal, "boom")}, wantStatus: http.StatusServiceUnavailable, wantCalled: true},
		{name: "success sets user and continues", authHeader: "Bearer good", val: &fakeValidator{resp: okResp}, wantStatus: http.StatusOK, wantUserID: "7", wantCalled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotUserID string
			r := gin.New()
			r.Use(Middleware(tt.val))
			r.GET("/x", func(c *gin.Context) {
				gotUserID = c.GetString(CtxUserID)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.val.called != tt.wantCalled {
				t.Errorf("validator called = %v, want %v", tt.val.called, tt.wantCalled)
			}
			if tt.wantUserID != "" && gotUserID != tt.wantUserID {
				t.Errorf("user_id = %q, want %q", gotUserID, tt.wantUserID)
			}
		})
	}
}
