package httpnode

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/jobstop"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/bytedance/sonic"
)

// defaultTimeout is used when the profile leaves timeout_seconds at 0.
const defaultTimeout = 30 * time.Second

// maxBodyBytes caps how much of a response body is read, so a runaway endpoint
// cannot exhaust memory. 10 MiB is generous for an API payload.
const maxBodyBytes = 10 << 20

// Stops holds the node's requests in flight, filed by jobId as each is accepted,
// so a stop for one reaches it — see runHandler. Its other half, Stops.OnSignal,
// belongs on the plugin's signal port, which the binary owns: main registers it.
var Stops jobstop.Registry

// Register wires the HTTP node's `run` action onto the plugin. Call it before
// p.Start(). For a request to stop with its flow, the plugin's signal port must
// also carry Stops.OnSignal — see main.
func Register(p *sdkv1.Plugin) {
	p.AddAction(sdkv1.Action{
		Method:         "run",
		Title:          "Run",
		Description:    "Make an HTTP request to a REST API — method, URL, headers, query and body, with connection config (base URL, auth, default headers) from a settings profile. Every string field resolves {{$.a.b}} tokens against the live flow context.",
		Middleware:     sdkv1.Use(Stops.Middleware),
		RequestHandler: runHandler,
	})
}

// runHandler implements the HTTP node's single `run` action. It resolves the
// {{$...}} tokens in every string field against the live flow context, builds
// and sends the request, and commits the response (status, headers, parsed body)
// to the node's scope.
//
// A transport-level failure (bad URL, DNS, connection, timeout) is a hard error
// reported with DoneWithError. An HTTP response — including a 4xx/5xx — is a
// success from this node's point of view: the status is committed under `status`
// (and `ok` for 2xx) so a downstream Rule node can branch on it, rather than the
// node needing its own error port.
//
// A request stops with its flow. It may be a slow download, a long poll or a
// retry budget waiting out a Retry-After of a minute, and once the flow is gone
// nothing downstream will read the response — so the action runs through
// Stops.Middleware, and the job's context is cancelled when the runtime stops
// this job's process (a user stop, a stop command, the workflow's timeout, the
// idle window). It aborts the request in flight and any retry wait (see send).
// Only this job's stop: another flow's run of this node is filed under its own
// jobId.
//
// What it deliberately does NOT do is unsend a request. A stop that lands while
// a POST is on the wire cuts the node's side of it; whether the server went
// ahead and acted is not something this end can know, which is the same caveat
// retrying carries (see retry.go).
func runHandler(job sdkv1.Job) {
	ctx := job.Context()
	req, err := sdkv1.CastRequestTo[RunBody](job.Req.Data)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	b := req.Body

	method := strings.ToUpper(strings.TrimSpace(b.Method))
	if method == "" {
		method = http.MethodGet
	}

	// 1. Resolve every string field's {{$...}} tokens against the flow context.
	rawURL := resolveVars(job, b.URL)
	if strings.TrimSpace(rawURL) == "" {
		job.DoneWithError("no URL: the request has no URL to call")
		return
	}
	headers := resolveKVs(job, b.Headers)
	defaults := resolveKVs(job, b.Settings.Headers)
	query := resolveKVs(job, b.Query)
	bodyStr := resolveVars(job, b.Body)

	settings := b.Settings
	settings.BaseURL = resolveVars(job, settings.BaseURL)
	settings.Username = resolveVars(job, settings.Username)
	settings.Password = resolveVars(job, settings.Password)
	settings.Token = resolveVars(job, settings.Token)

	// 2. Assemble the final URL (base + path + query).
	finalURL, err := buildURL(settings.BaseURL, rawURL, query)
	if err != nil {
		job.DoneWithError("invalid URL: " + err.Error())
		return
	}

	job.Progress(20, sdkv1.Frame{
		Title:   "http",
		Content: fmt.Sprintf("%s %s", method, finalURL),
	})

	// 3. Build the request. A body is sent for any method that carries one; GET
	//    with a non-empty body is unusual but not forbidden, so honour whatever
	//    the drawer set.
	hasBody := strings.TrimSpace(bodyStr) != ""
	var bodyReader io.Reader
	if hasBody {
		bodyReader = strings.NewReader(bodyStr)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, finalURL, bodyReader)
	if err != nil {
		job.DoneWithError("build request: " + err.Error())
		return
	}
	applyHeaders(httpReq.Header, defaults, headers, b.BodyType, hasBody)
	applyAuth(httpReq.Header, settings)

	// 4. Send it.
	client := newClient(settings)
	resp, err := send(client, httpReq, retriesOf(settings), func(note string) {
		job.Progress(60, sdkv1.Frame{Title: "retrying", Content: note})
	})
	if err != nil {
		if ctx.Err() != nil {
			// Stopped, not failed: the runtime has concluded this process and
			// stopped listening, so there is nothing to report the failure to —
			// and every command sent now would only wait out the SDK's retries.
			return
		}
		job.DoneWithError("request failed: " + err.Error())
		return
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		if ctx.Err() != nil {
			return // stopped mid-body: wind down quietly
		}
		job.DoneWithError("read response: " + err.Error())
		return
	}

	job.Progress(80, sdkv1.Frame{
		Title:   "http",
		Content: fmt.Sprintf("%d %s (%d bytes)", resp.StatusCode, http.StatusText(resp.StatusCode), len(raw)),
	})

	// 5. Commit the response as the node's scope. The body is decoded to its
	//    natural type only for JSON responses (so downstream nodes read fields
	//    directly); anything else stays the raw string.
	result := httpResult{
		Method:  method,
		URL:     finalURL,
		Status:  resp.StatusCode,
		OK:      resp.StatusCode >= 200 && resp.StatusCode < 300,
		Headers: flattenHeaders(resp.Header),
		Body:    decodeBody(resp.Header.Get("Content-Type"), raw),
	}
	out, err := toMap(result)
	if err != nil {
		job.DoneWithError("encode result: " + err.Error())
		return
	}
	job.Done(out)
}

// newClient builds the HTTP client for one request: the profile's timeout (or
// the default), and a transport that optionally skips TLS verification.
func newClient(s HTTPSettings) *http.Client {
	timeout := defaultTimeout
	if s.TimeoutSeconds > 0 {
		timeout = time.Duration(s.TimeoutSeconds) * time.Second
	}
	client := &http.Client{Timeout: timeout}
	if s.InsecureSkipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return client
}

// decodeBody returns the response body as a typed value for JSON content and as
// a plain string otherwise. A JSON content-type whose body fails to parse falls
// back to the raw string rather than dropping it.
func decodeBody(contentType string, raw []byte) any {
	if len(raw) == 0 {
		return ""
	}
	if strings.Contains(strings.ToLower(contentType), "json") {
		var v any
		if err := sonic.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	return string(raw)
}

// toMap round-trips the result struct through JSON into the map[string]any the
// Done payload (and thus the node scope) is built from.
func toMap(result httpResult) (map[string]any, error) {
	b, err := sonic.Marshal(result)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := sonic.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
