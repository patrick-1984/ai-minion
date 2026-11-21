// Command usageschema prints the public usage operations as OpenAPI 3.1.
// Redirect stdout to research/minion-usage/openapi.json after changing a DTO.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

type object = map[string]any

func schema(t reflect.Type) *jsonschema.Schema {
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		panic(err)
	}
	return s
}

func main() {
	paths := object{}
	components := object{}
	for _, name := range []string{ops.OpUsageList, ops.OpUsageBucketList, ops.OpUsageBucketSet, ops.OpWorkerUsage} {
		op, ok := ops.Default.Lookup(name)
		if !ok {
			panic(name)
		}
		for _, t := range []reflect.Type{op.Args, op.Result} {
			components[t.Name()] = schema(t)
		}
		content := func(t reflect.Type) object {
			return object{"application/json": object{"schema": object{"$ref": "#/components/schemas/" + t.Name()}}}
		}
		paths["/api/v1/ops/"+name] = object{"post": object{
			"operationId": name, "summary": op.Summary, "requestBody": object{"required": true, "content": content(op.Args)},
			"responses": object{"200": object{"description": "Authorized usage result", "content": content(op.Result)}, "default": object{"description": "Hub API error", "content": object{"application/json": object{"schema": object{"$ref": "#/components/schemas/HubError"}}}}},
		}}
	}
	components["HubError"] = object{"type": "object", "required": []string{"error"}, "properties": object{"error": object{"type": "object", "properties": object{"code": object{"type": "string"}, "message": object{"type": "string"}}}}}
	spec := object{"openapi": "3.1.1", "info": object{"title": "Minion usage", "version": "1", "description": "All telemetry timestamps use Unix milliseconds UTC. Null counters and limits mean unavailable."}, "paths": paths, "security": []object{{"bearer": []string{}}}, "components": object{"schemas": components, "securitySchemes": object{"bearer": object{"type": "http", "scheme": "bearer"}}}}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
