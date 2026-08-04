package httpnode

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

// contentTypeFor maps the drawer's body_type to a default Content-Type. An
// explicit Content-Type header (profile or per-request) always wins over this,
// and an unknown/blank type contributes nothing.
func contentTypeFor(bodyType string) string {
	switch strings.ToLower(strings.TrimSpace(bodyType)) {
	case "json":
		return "application/json"
	case "form":
		return "application/x-www-form-urlencoded"
	case "text":
		return "text/plain"
	}
	return ""
}

// buildURL joins a (possibly relative) request URL to the profile base URL and
// appends the query parameters. A request URL that already has a scheme
// (http:// or https://) is treated as absolute and the base URL is ignored;
// otherwise base + path are concatenated with exactly one slash between them.
// Query pairs are added to whatever query string the URL already carried.
func buildURL(baseURL, reqURL string, query []KV) (string, error) {
	full := strings.TrimSpace(reqURL)
	if !hasScheme(full) {
		base := strings.TrimSpace(baseURL)
		if base != "" {
			full = strings.TrimRight(base, "/") + "/" + strings.TrimLeft(full, "/")
		}
	}
	u, err := url.Parse(full)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		q := u.Query()
		for _, p := range query {
			q.Add(p.Key, p.Value)
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// hasScheme reports whether a URL already names http/https, i.e. is absolute.
func hasScheme(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// applyHeaders sets the profile's default headers first, then the per-request
// headers (so a per-request header overrides a default of the same name), then
// a default Content-Type from bodyType when a body is present and no
// Content-Type was set explicitly.
func applyHeaders(h http.Header, defaults, perReq []KV, bodyType string, hasBody bool) {
	for _, kv := range defaults {
		h.Set(kv.Key, kv.Value)
	}
	for _, kv := range perReq {
		h.Set(kv.Key, kv.Value)
	}
	if hasBody && h.Get("Content-Type") == "" {
		if ct := contentTypeFor(bodyType); ct != "" {
			h.Set("Content-Type", ct)
		}
	}
}

// applyAuth adds the credential named by the profile's auth type. Credentials
// are taken already-resolved (the handler resolves their {{$...}} tokens first).
// An auth header the caller already set via a header row is not overwritten, so
// a hand-set Authorization always wins.
func applyAuth(h http.Header, s HTTPSettings) {
	switch strings.ToLower(strings.TrimSpace(s.AuthType)) {
	case "basic":
		if h.Get("Authorization") != "" {
			return
		}
		token := base64.StdEncoding.EncodeToString([]byte(s.Username + ":" + s.Password))
		h.Set("Authorization", "Basic "+token)
	case "bearer":
		if h.Get("Authorization") != "" {
			return
		}
		h.Set("Authorization", "Bearer "+s.Token)
	case "api_key":
		name := strings.TrimSpace(s.HeaderName)
		if name == "" {
			name = "Authorization"
		}
		if h.Get(name) != "" {
			return
		}
		h.Set(name, s.Token)
	}
}

// flattenHeaders collapses a response header map into a plain string map,
// joining a header's multiple values with ", " (the standard field-folding
// separator), so the committed result is a flat, readable object.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}
