package truenas

import (
	"context"
	"encoding/json"
	"fmt"
)

// clientScript runs inside the TrueNAS host's Python with the built-in
// middleware client, which talks to the local middlewared socket (no network
// credential). The request (method and parameters, possibly the whole compose
// document) arrives on stdin, never in argv; the script text is fixed. Only
// an exception class name leaves the script on failure.
const clientScript = `import json, sys
req = json.load(sys.stdin)
try:
    from truenas_api_client import Client
    with Client() as c:
        r = c.call(req["method"], *req.get("params", []))
    sys.stdout.write(json.dumps({"result": r}))
except Exception as e:
    sys.stdout.write(json.dumps({"error": e.__class__.__name__}))
`

// MidcltClient implements [Middleware] over the local TrueNAS client.
type MidcltClient struct {
	Runner Runner
	// Python is the interpreter ("python3" when empty).
	Python string
}

// MiddlewareError is a failed middleware call; it carries the method and a
// short class, never a value from the request or the answer.
type MiddlewareError struct {
	Method string
	Class  string
}

func (e *MiddlewareError) Error() string {
	return fmt.Sprintf("middleware %s failed (%s)", e.Method, e.Class)
}

func (m *MidcltClient) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	req, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, err
	}
	py := m.Python
	if py == "" {
		py = "python3"
	}
	out, err := m.Runner.Run(ctx, req, py, "-I", "-c", clientScript)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, &MiddlewareError{Method: method, Class: "client_exit"}
	}
	var ans struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(out, &ans); err != nil {
		return nil, &MiddlewareError{Method: method, Class: "bad_answer"}
	}
	if ans.Error != "" {
		return nil, &MiddlewareError{Method: method, Class: sanitizeClass(ans.Error)}
	}
	return ans.Result, nil
}

func sanitizeClass(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "error"
		}
	}
	return s
}

func (m *MidcltClient) queryApp(ctx context.Context, app string) (map[string]any, error) {
	raw, err := m.call(ctx, "app.query", [][]any{{"name", "=", app}})
	if err != nil {
		return nil, err
	}
	var apps []map[string]any
	if err := json.Unmarshal(raw, &apps); err != nil || len(apps) != 1 {
		return nil, &MiddlewareError{Method: "app.query", Class: "not_found"}
	}
	return apps[0], nil
}

// AppState implements Middleware.
func (m *MidcltClient) AppState(ctx context.Context, app string) (string, error) {
	a, err := m.queryApp(ctx, app)
	if err != nil {
		return "", err
	}
	s, _ := a["state"].(string)
	return s, nil
}

// AppConfig implements Middleware: the complete compose document of a custom
// app (the "custom_compose_config" of app.config).
func (m *MidcltClient) AppConfig(ctx context.Context, app string) (Compose, error) {
	raw, err := m.call(ctx, "app.config", app)
	if err != nil {
		return nil, err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, &MiddlewareError{Method: "app.config", Class: "bad_answer"}
	}
	doc, ok := cfg["custom_compose_config"]
	if !ok {
		return nil, &MiddlewareError{Method: "app.config", Class: "not_a_custom_app"}
	}
	var c Compose
	if err := json.Unmarshal(doc, &c); err != nil || c == nil {
		return nil, &MiddlewareError{Method: "app.config", Class: "bad_answer"}
	}
	return c, nil
}

func (m *MidcltClient) submit(ctx context.Context, method string, params ...any) (int, error) {
	raw, err := m.call(ctx, method, params...)
	if err != nil {
		return 0, err
	}
	var id int
	if err := json.Unmarshal(raw, &id); err != nil || id <= 0 {
		return 0, &MiddlewareError{Method: method, Class: "no_job_id"}
	}
	return id, nil
}

// Stop implements Middleware (app.stop; a job).
func (m *MidcltClient) Stop(ctx context.Context, app string) (int, error) {
	return m.submit(ctx, "app.stop", app)
}

// Start implements Middleware (app.start; a job).
func (m *MidcltClient) Start(ctx context.Context, app string) (int, error) {
	return m.submit(ctx, "app.start", app)
}

// Update implements Middleware (app.update with the full custom compose).
func (m *MidcltClient) Update(ctx context.Context, app string, c Compose) (int, error) {
	return m.submit(ctx, "app.update", app, map[string]any{"custom_compose_config": map[string]any(c)})
}

// JobState implements Middleware (core.get_jobs).
func (m *MidcltClient) JobState(ctx context.Context, id int) (JobState, error) {
	raw, err := m.call(ctx, "core.get_jobs", [][]any{{"id", "=", id}})
	if err != nil {
		return "", err
	}
	var jobs []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return "", &MiddlewareError{Method: "core.get_jobs", Class: "bad_answer"}
	}
	if len(jobs) == 0 {
		return JobUnknown, nil
	}
	return JobState(jobs[0].State), nil
}

var _ Middleware = (*MidcltClient)(nil)
