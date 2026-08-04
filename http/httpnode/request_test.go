package httpnode

import (
	"net/http"
	"testing"
)

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		reqURL  string
		query   []KV
		want    string
		wantErr bool
	}{
		{name: "absolute url ignores base", base: "https://api.example.com", reqURL: "https://other.com/x", want: "https://other.com/x"},
		{name: "relative path joins base", base: "https://api.example.com", reqURL: "users/1", want: "https://api.example.com/users/1"},
		{name: "single slash between base and path", base: "https://api.example.com/", reqURL: "/users/1", want: "https://api.example.com/users/1"},
		{name: "no base keeps relative", base: "", reqURL: "https://api.example.com/x", want: "https://api.example.com/x"},
		{name: "query appended", base: "https://api.example.com", reqURL: "/search", query: []KV{{Key: "q", Value: "go lang"}}, want: "https://api.example.com/search?q=go+lang"},
		{name: "query merges with existing", base: "", reqURL: "https://api.example.com/x?a=1", query: []KV{{Key: "b", Value: "2"}}, want: "https://api.example.com/x?a=1&b=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildURL(tt.base, tt.reqURL, tt.query)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildURL err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("buildURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyAuth(t *testing.T) {
	t.Run("basic builds Base64 header", func(t *testing.T) {
		h := http.Header{}
		applyAuth(h, HTTPSettings{AuthType: "basic", Username: "ada", Password: "s3cret"})
		// base64("ada:s3cret") == YWRhOnMzY3JldA==
		if got := h.Get("Authorization"); got != "Basic YWRhOnMzY3JldA==" {
			t.Errorf("Authorization = %q", got)
		}
	})
	t.Run("bearer", func(t *testing.T) {
		h := http.Header{}
		applyAuth(h, HTTPSettings{AuthType: "bearer", Token: "tok"})
		if got := h.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
	})
	t.Run("api_key uses custom header", func(t *testing.T) {
		h := http.Header{}
		applyAuth(h, HTTPSettings{AuthType: "api_key", HeaderName: "X-Api-Key", Token: "abc"})
		if got := h.Get("X-Api-Key"); got != "abc" {
			t.Errorf("X-Api-Key = %q", got)
		}
	})
	t.Run("api_key defaults to Authorization", func(t *testing.T) {
		h := http.Header{}
		applyAuth(h, HTTPSettings{AuthType: "api_key", Token: "abc"})
		if got := h.Get("Authorization"); got != "abc" {
			t.Errorf("Authorization = %q", got)
		}
	})
	t.Run("existing auth header wins", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer manual")
		applyAuth(h, HTTPSettings{AuthType: "basic", Username: "ada", Password: "x"})
		if got := h.Get("Authorization"); got != "Bearer manual" {
			t.Errorf("Authorization = %q, want the manual value untouched", got)
		}
	})
	t.Run("none adds nothing", func(t *testing.T) {
		h := http.Header{}
		applyAuth(h, HTTPSettings{AuthType: "none"})
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
	})
}

func TestApplyHeaders(t *testing.T) {
	t.Run("per-request overrides default and sets content-type", func(t *testing.T) {
		h := http.Header{}
		applyHeaders(h,
			[]KV{{Key: "X-Env", Value: "prod"}, {Key: "Accept", Value: "text/plain"}},
			[]KV{{Key: "Accept", Value: "application/json"}},
			"json", true)
		if got := h.Get("X-Env"); got != "prod" {
			t.Errorf("X-Env = %q", got)
		}
		if got := h.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want per-request override", got)
		}
		if got := h.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	})
	t.Run("explicit content-type is not overridden", func(t *testing.T) {
		h := http.Header{}
		applyHeaders(h, nil, []KV{{Key: "Content-Type", Value: "application/xml"}}, "json", true)
		if got := h.Get("Content-Type"); got != "application/xml" {
			t.Errorf("Content-Type = %q, want the explicit value", got)
		}
	})
	t.Run("no content-type without a body", func(t *testing.T) {
		h := http.Header{}
		applyHeaders(h, nil, nil, "json", false)
		if got := h.Get("Content-Type"); got != "" {
			t.Errorf("Content-Type = %q, want none for a bodyless request", got)
		}
	})
}

func TestDecodeBody(t *testing.T) {
	t.Run("json object decodes to a map", func(t *testing.T) {
		v := decodeBody("application/json; charset=utf-8", []byte(`{"a":1}`))
		m, ok := v.(map[string]any)
		if !ok || m["a"] != float64(1) {
			t.Errorf("decodeBody = %#v, want map with a=1", v)
		}
	})
	t.Run("non-json stays a string", func(t *testing.T) {
		v := decodeBody("text/plain", []byte("hello"))
		if v != "hello" {
			t.Errorf("decodeBody = %#v, want \"hello\"", v)
		}
	})
	t.Run("invalid json falls back to string", func(t *testing.T) {
		v := decodeBody("application/json", []byte("not json"))
		if v != "not json" {
			t.Errorf("decodeBody = %#v, want the raw string", v)
		}
	})
	t.Run("empty body is an empty string", func(t *testing.T) {
		if v := decodeBody("application/json", nil); v != "" {
			t.Errorf("decodeBody = %#v, want empty string", v)
		}
	})
}
