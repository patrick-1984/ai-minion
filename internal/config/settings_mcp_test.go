package config

import "testing"

func TestMCPAllowWritesDeclared(t *testing.T) {
	s, ok := Default.Lookup(KeyMCPAllowWrites)
	if !ok {
		t.Fatal("mcp_allow_writes not declared")
	}
	if s.Scope != ScopeWorker || s.EnvName() != "MINION_MCP_ALLOW_WRITES" || s.Default != "false" || s.Help == "" {
		t.Fatalf("mcp_allow_writes: %+v", s)
	}
	for _, h := range Default.ForScope(ScopeHub) {
		if h.Key == KeyMCPAllowWrites {
			t.Error("the Hub can resolve mcp_allow_writes")
		}
	}
}

func TestResolveMCPAllowWrites(t *testing.T) {
	cases := []struct {
		name  string
		flags map[string]string
		env   map[string]string
		want  bool
		err   bool
	}{
		{"default is read-only", nil, nil, false, false},
		{"environment", nil, map[string]string{"MINION_MCP_ALLOW_WRITES": "true"}, true, false},
		{"flag beats environment", map[string]string{KeyMCPAllowWrites: "false"}, map[string]string{"MINION_MCP_ALLOW_WRITES": "true"}, false, false},
		{"invalid value", nil, map[string]string{"MINION_MCP_ALLOW_WRITES": "yes"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveMCPAllowWrites(workerResolver(c.flags, c.env, nil))
			if (err != nil) != c.err || got != c.want {
				t.Fatalf("got %v, %v; want %v (error %v)", got, err, c.want, c.err)
			}
		})
	}
}
