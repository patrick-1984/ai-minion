package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

type usageOpenAPIRef struct {
	Schema struct {
		Ref string `json:"$ref"`
	} `json:"schema"`
}

type usageOpenAPIContent struct {
	Content map[string]usageOpenAPIRef `json:"content"`
}

func TestUsageOpenAPIReconcilesOperationRefsAndNullableDTOs(t *testing.T) {
	raw, err := os.ReadFile("../../research/minion-usage/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string                         `json:"operationId"`
			RequestBody usageOpenAPIContent            `json:"requestBody"`
			Responses   map[string]usageOpenAPIContent `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	use := func(name string) *jsonschema.Resolved {
		t.Helper()
		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.Components.Schemas[name], &schema); err != nil {
			t.Fatalf("missing or invalid component %s: %v", name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatalf("%s: unresolved nested references: %v", name, err)
		}
		return resolved
	}
	for _, name := range []string{ops.OpUsageList, ops.OpUsageBucketList, ops.OpUsageReadback, ops.OpUsageBucketSet, ops.OpUsageAccountSet, ops.OpUsageCollectorSet, ops.OpWorkerUsage} {
		op, ok := ops.Lookup(name)
		if !ok {
			t.Fatalf("operation %q is missing from the actual catalog", name)
		}
		post := spec.Paths[ops.RESTPrefix+name]["post"]
		if post.OperationID != name {
			t.Errorf("OpenAPI POST for %s uses operation ID %q", name, post.OperationID)
		}
		for _, c := range []struct {
			name string
			body usageOpenAPIContent
		}{{op.Args.Name(), post.RequestBody}, {op.Result.Name(), post.Responses["200"]}, {"HubError", post.Responses["default"]}} {
			ref := c.body.Content["application/json"].Schema.Ref
			if ref != "#/components/schemas/"+c.name {
				t.Errorf("%s points to %q, expected component %s", name, ref, c.name)
			}
			use(c.name)
		}
	}
	check := func(name string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var actual any
		if err := json.Unmarshal(b, &actual); err != nil {
			t.Fatal(err)
		}
		if err := use(name).Validate(actual); err != nil {
			t.Errorf("%s rejects the actual DTO JSON (including explicit zero/null): %v; %s", name, err, b)
		}
	}
	zero := 0.
	tokens := int64(0)
	check("UsageListResult", ops.UsageListResult{
		Version: ops.UsageVersion, AsOf: 1_800_000_000_000,
		Sessions: []ops.UsageSessionRow{{UsageSession: ops.UsageSession{UsageCounters: ops.UsageCounters{InputTokens: &tokens, CostUSD: &zero}}}},
		Workers:  []ops.UsageWorkerRow{{}},
		Accounts: []ops.UsageAccountRow{
			{UsageAccount: ops.UsageAccount{Windows: []ops.UsageWindow{{UsedPercent: &zero}, {UsedPercent: nil}}}, Collector: &ops.UsageCollector{Mode: "passive_events"}},
			{UsageAccount: ops.UsageAccount{Windows: nil}},
		},
	})
	check("UsageReport", ops.UsageReport{Version: ops.UsageVersion, ObservedAt: 1_800_000_000_000})
	check("UsageCollectorSetArgs", ops.UsageCollectorSetArgs{AccountID: strings.Repeat("0", 64), Start: true})
	check("UsageCollector", ops.UsageCollector{Mode: "native_background", CanStart: true})
	check("WorkerUsageArgs", ops.WorkerUsageArgs{WorkerID: ops.NewUUIDv7(), Inventory: true})
	workerRequest := use("WorkerUsageArgs")
	for internal, value := range map[string]any{"account_id": strings.Repeat("1", 64), "after_event": float64(1)} {
		if err := workerRequest.Validate(map[string]any{"worker_id": ops.NewUUIDv7(), internal: value}); err == nil {
			t.Errorf("OpenAPI documents %s as a public WorkerUsageArgs member", internal)
		}
	}
}
