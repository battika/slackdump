package renderer

import (
	"strings"
	"testing"
)

func TestRoutes_RewriteSlackURL(t *testing.T) {
	routes := NewRoutes(ModeLive,
		WithWorkspaceURL("https://example.com"),
		WithLiveHost("localhost:8080"),
	)

	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "channel link",
			src:  "https://example.com/archives/C12341",
			want: "/archives/C12341",
		},
		{
			name: "thread permalink",
			src:  "https://example.com/archives/C12341/p1738580940349469?thread_ts=1737716342.919259&cid=C12341",
			want: "/archives/C12341/1737716342.919259#1738580940.349469",
		},
		{
			name: "channel anchor permalink",
			src:  "https://example.com/archives/C12341/p1738580940349469",
			want: "/archives/C12341#1738580940.349469",
		},
		{
			name: "user link",
			src:  "https://example.com/team/U123",
			want: "/team/U123",
		},
		{
			name: "fallback host replacement",
			src:  "https://example.com/help",
			want: "http://localhost:8080/help",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routes.RewriteSlackURL(tt.src); got != tt.want {
				t.Fatalf("RewriteSlackURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoutes_Avatar(t *testing.T) {
	t.Run("live mode", func(t *testing.T) {
		r := NewRoutes(ModeLive)
		if got := r.Avatar("U123", "abc.jpg"); got != "/avatars/U123/abc.jpg" {
			t.Fatalf("Avatar() = %q, want %q", got, "/avatars/U123/abc.jpg")
		}
	})
	t.Run("static mode", func(t *testing.T) {
		r := NewRoutes(ModeStatic)
		if got := r.Avatar("U123", "abc.png"); got != "/avatars/U123/abc.png" {
			t.Fatalf("Avatar() = %q, want %q", got, "/avatars/U123/abc.png")
		}
	})
}

func TestRoutes_StaticPaths(t *testing.T) {
	routes := NewRoutes(ModeStatic)

	if got := routes.Channel("C123"); got != "/archives/C123/index.html" {
		t.Fatalf("Channel() = %q", got)
	}
	if got := routes.Thread("C123", "1710000000.000001"); got != "/archives/C123/threads/1710000000.000001.html" {
		t.Fatalf("Thread() = %q", got)
	}
	if got := routes.Canvas("C123"); got != "/archives/C123/canvas/index.html" {
		t.Fatalf("Canvas() = %q", got)
	}
	if got := routes.CanvasContent("C123"); got != "/archives/C123/canvas/content.html" {
		t.Fatalf("CanvasContent() = %q", got)
	}
	if got := routes.File("F123", "hello world.txt"); got != "/files/F123/hello%20world.txt" {
		t.Fatalf("File() = %q", got)
	}
	if got := routes.File("F123", "a/b:c.txt"); got != "/files/F123/a_b_c.txt" {
		t.Fatalf("File() sanitized = %q", got)
	}
}

func TestRoutes_ChannelPage(t *testing.T) {
	tests := []struct {
		name string
		rts  *Routes
		id   string
		page int
		want string
	}{
		{"unpaged live", NewRoutes(ModeLive), "C1", 3, "/archives/C1"},
		{"paged live", NewRoutes(ModeLive, WithPaging(true)), "C1", 3, "/archives/C1?p=3"},
		{"paged live first page", NewRoutes(ModeLive, WithPaging(true)), "C1", 1, "/archives/C1?p=1"},
		{"paged live page zero omits param", NewRoutes(ModeLive, WithPaging(true)), "C1", 0, "/archives/C1"},
		{"static ignores paging", NewRoutes(ModeStatic, WithPaging(true)), "C1", 3, "/archives/C1/index.html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rts.ChannelPage(tt.id, tt.page); got != tt.want {
				t.Errorf("ChannelPage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoutes_ChannelPageMessage(t *testing.T) {
	r := NewRoutes(ModeLive, WithPaging(true))
	const want = "/archives/C1?p=42#1738580940.349469"
	got := r.ChannelPageMessage("C1", 42, "1738580940.349469")
	if got != want {
		t.Fatalf("ChannelPageMessage() = %q, want %q", got, want)
	}
	// The handler-facing form must be terminal: routing it back through the
	// p-form redirect handler would loop forever.
	if strings.Contains(got, "/p1738580940349469") {
		t.Fatalf("ChannelPageMessage() must not return the p-form: %q", got)
	}
}

func TestRoutes_ChannelMessage(t *testing.T) {
	tests := []struct {
		name string
		rts  *Routes
		want string
	}{
		{"unpaged anchors directly", NewRoutes(ModeLive), "/archives/C1#1738580940.349469"},
		{"paged routes through the redirect handler", NewRoutes(ModeLive, WithPaging(true)), "/archives/C1/p1738580940349469"},
		{"static anchors directly", NewRoutes(ModeStatic, WithPaging(true)), "/archives/C1/index.html#1738580940.349469"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rts.ChannelMessage("C1", "1738580940.349469"); got != tt.want {
				t.Errorf("ChannelMessage() = %q, want %q", got, tt.want)
			}
		})
	}

	// A timestamp TStoThreadID cannot convert must fall back to the anchor
	// form rather than emitting a truncated p-link.
	t.Run("malformed timestamp falls back to the anchor form", func(t *testing.T) {
		r := NewRoutes(ModeLive, WithPaging(true))
		const want = "/archives/C1#notatimestamp"
		if got := r.ChannelMessage("C1", "notatimestamp"); got != want {
			t.Errorf("ChannelMessage() = %q, want %q", got, want)
		}
	})
}
