package truenas

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// clientScript runs inside the TrueNAS host's Python with the built-in
// middleware client, which talks to the local middlewared socket (no network
// credential). The request (method and parameters, possibly the whole compose
// document) arrives on stdin, never in argv; the script text is fixed. Only
// an exception class name leaves the script on failure.
const clientScript = `import json, sys
req = json.load(sys.stdin)
def jobs(r):
    out = []
    for j in r:
        a = j.get("arguments") or []
        st = j.get("time_started")
        out.append({"id": j.get("id"), "state": j.get("state"), "method": j.get("method"),
                    "app": a[0] if a and isinstance(a[0], str) else None,
                    "started": st.timestamp() if hasattr(st, "timestamp") else None})
    return out
try:
    from truenas_api_client import Client
    with Client() as c:
        r = c.call(req["method"], *req.get("params", []))
    if req.get("project") == "jobs":
        r = jobs(r)
    sys.stdout.write(json.dumps({"result": r}, default=str))
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
	return m.callProject(ctx, "", method, params...)
}

func (m *MidcltClient) callProject(ctx context.Context, project, method string, params ...any) (json.RawMessage, error) {
	req, err := json.Marshal(map[string]any{"method": method, "params": params, "project": project})
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

// AppConfig implements Middleware: app.config returns the saved compose
// document of a custom app directly (TrueNAS 24.10 to 25.10). A document
// wrapped in "custom_compose_config" is accepted too.
func (m *MidcltClient) AppConfig(ctx context.Context, app string) (Compose, error) {
	raw, err := m.call(ctx, "app.config", app)
	if err != nil {
		return nil, err
	}
	var c Compose
	if err := json.Unmarshal(raw, &c); err != nil || c == nil {
		return nil, &MiddlewareError{Method: "app.config", Class: "bad_answer"}
	}
	if _, ok := c["services"]; !ok {
		if inner, ok := c["custom_compose_config"].(map[string]any); ok {
			c = Compose(inner)
		}
	}
	if _, ok := c["services"]; !ok {
		return nil, &MiddlewareError{Method: "app.config", Class: "not_a_custom_app"}
	}
	return c, nil
}

// IsCustom implements Middleware.
func (m *MidcltClient) IsCustom(ctx context.Context, app string) (bool, error) {
	a, err := m.queryApp(ctx, app)
	if err != nil {
		return false, err
	}
	custom, _ := a["custom_app"].(bool)
	return custom, nil
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

type jobRow struct {
	ID      int      `json:"id"`
	State   string   `json:"state"`
	App     *string  `json:"app"`
	Started *float64 `json:"started"`
}

func (m *MidcltClient) jobs(ctx context.Context, filters [][]any, opts map[string]any) ([]jobRow, error) {
	raw, err := m.callProject(ctx, "jobs", "core.get_jobs", filters, opts)
	if err != nil {
		return nil, err
	}
	var rows []jobRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, &MiddlewareError{Method: "core.get_jobs", Class: "bad_answer"}
	}
	return rows, nil
}

// JobState implements Middleware (core.get_jobs).
func (m *MidcltClient) JobState(ctx context.Context, id int) (JobState, error) {
	rows, err := m.jobs(ctx, [][]any{{"id", "=", id}}, map[string]any{})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return JobUnknown, nil
	}
	return JobState(rows[0].State), nil
}

// FindJobs implements Middleware.
func (m *MidcltClient) FindJobs(ctx context.Context, method, app string, since time.Time) ([]JobInfo, error) {
	rows, err := m.jobs(ctx, [][]any{{"method", "=", method}}, map[string]any{"order_by": []string{"-id"}, "limit": 25})
	if err != nil {
		return nil, err
	}
	var out []JobInfo
	for _, r := range rows {
		if r.App == nil || *r.App != app {
			continue
		}
		// A job with no start time is still waiting: it counts.
		if r.Started != nil && time.Unix(int64(*r.Started), 0).Before(since.Add(-time.Minute)) {
			continue
		}
		out = append(out, JobInfo{ID: r.ID, State: JobState(r.State)})
	}
	return out, nil
}

var _ Middleware = (*MidcltClient)(nil)
