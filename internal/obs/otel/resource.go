package otel

import (
	"context"
	"errors"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Process describes the process that exports, for the resource of its
// records.
type Process struct {
	// Service is minion-hub or minion-worker (service.name).
	Service string
	// Version is service.version.
	Version string
	// InstanceID is service.instance.id: the Hub's stable id, the Worker's
	// id once enrolled, else a random id per start.
	InstanceID string
	// WorkerID is minion.worker.id (Workers only).
	WorkerID string
}

// ErrResourceEnv is returned (with a usable resource) when
// OTEL_RESOURCE_ATTRIBUTES or OTEL_SERVICE_NAME cannot be parsed. Its text
// never contains the value.
var ErrResourceEnv = errors.New("OTEL_RESOURCE_ATTRIBUTES could not be parsed and was ignored")

// NewResource returns the resource of p: service.name, service.version,
// service.instance.id, host.name and minion.worker.id, then the values of
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME (which win, as the
// specification says). A parse error of the environment yields the resource
// without it and [ErrResourceEnv].
func NewResource(ctx context.Context, p Process) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{attribute.String("service.name", p.Service)}
	if p.Version != "" {
		attrs = append(attrs, attribute.String("service.version", p.Version))
	}
	if p.InstanceID != "" {
		attrs = append(attrs, attribute.String("service.instance.id", p.InstanceID))
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		attrs = append(attrs, attribute.String("host.name", h))
	}
	if p.WorkerID != "" {
		attrs = append(attrs, attribute.String("minion.worker.id", p.WorkerID))
	}
	base := resource.NewSchemaless(attrs...)
	env, err := resource.New(ctx, resource.WithFromEnv())
	if err != nil {
		return base, ErrResourceEnv
	}
	merged, err := resource.Merge(base, env)
	if err != nil {
		return base, nil
	}
	return merged, nil
}
