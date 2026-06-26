package grpcx

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestWithAuthToken(t *testing.T) {
	t.Parallel()

	t.Run("blank token is a no-op", func(t *testing.T) {
		t.Parallel()
		ctx := WithAuthToken(context.Background(), "")
		if _, ok := metadata.FromOutgoingContext(ctx); ok {
			t.Error("blank token must not attach outgoing metadata")
		}
	})

	t.Run("appends authorization metadata", func(t *testing.T) {
		t.Parallel()
		ctx := WithAuthToken(context.Background(), "Bearer abc")
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			t.Fatal("expected outgoing metadata to be set")
		}
		if got := md.Get(authMetadataKey); len(got) != 1 || got[0] != "Bearer abc" {
			t.Errorf("%s = %v, want [Bearer abc]", authMetadataKey, got)
		}
	})
}

func TestTokenFromContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		ctx    func() context.Context
		want   string
		wantOK bool
	}{
		{
			name: "no incoming metadata",
			ctx:  context.Background,
			want: "", wantOK: false,
		},
		{
			name: "token present",
			ctx: func() context.Context {
				return metadata.NewIncomingContext(context.Background(),
					metadata.Pairs(authMetadataKey, "Bearer xyz"))
			},
			want: "Bearer xyz", wantOK: true,
		},
		{
			name: "key present but empty",
			ctx: func() context.Context {
				return metadata.NewIncomingContext(context.Background(),
					metadata.Pairs(authMetadataKey, ""))
			},
			want: "", wantOK: false,
		},
		{
			name: "different key only",
			ctx: func() context.Context {
				return metadata.NewIncomingContext(context.Background(),
					metadata.Pairs("other", "v"))
			},
			want: "", wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := TokenFromContext(tt.ctx())
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("TokenFromContext() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
