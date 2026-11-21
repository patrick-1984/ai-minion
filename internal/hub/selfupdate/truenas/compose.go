package truenas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// Compose is the complete compose document of a custom app, as the
// middleware stores it. It may hold secrets: it is never logged and never put
// into a command line.
type Compose map[string]any

// Hash is the SHA-256 of the canonical JSON (Go sorts map keys).
func (c Compose) Hash() string {
	b, _ := json.Marshal(map[string]any(c))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Image returns the image reference of a service.
func (c Compose) Image(service string) (string, error) {
	svc, err := c.service(service)
	if err != nil {
		return "", err
	}
	img, _ := svc["image"].(string)
	if img == "" {
		return "", errors.New("service has no image")
	}
	return img, nil
}

func (c Compose) service(name string) (map[string]any, error) {
	svcs, _ := c["services"].(map[string]any)
	svc, _ := svcs[name].(map[string]any)
	if svc == nil {
		return nil, errors.New("service not found in the compose document")
	}
	return svc, nil
}

// WithImage returns a deep copy with the image of service replaced.
func (c Compose) WithImage(service, ref string) (Compose, error) {
	out, err := clone(c)
	if err != nil {
		return nil, err
	}
	svc, err := out.service(service)
	if err != nil {
		return nil, err
	}
	svc["image"] = ref
	return out, nil
}

// MaintenanceEnv is the Hub setting that makes it reject every request that
// could change state (see the Hub's maintenance gate) until it is removed.
const (
	MaintenanceEnv   = "MINION_MAINTENANCE"
	MaintenanceValue = "readonly"
)

func clone(c Compose) (Compose, error) {
	b, err := json.Marshal(map[string]any(c))
	if err != nil {
		return nil, err
	}
	var out Compose
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Env returns the value of an environment variable of a service (map or list form).
func (c Compose) Env(service, key string) string {
	svc, err := c.service(service)
	if err != nil {
		return ""
	}
	switch env := svc["environment"].(type) {
	case map[string]any:
		v, _ := env[key].(string)
		return v
	case []any:
		for _, e := range env {
			if kv, ok := e.(string); ok && strings.HasPrefix(kv, key+"=") {
				return kv[len(key)+1:]
			}
		}
	}
	return ""
}

// withEnv returns a deep copy with key set (value != "") or removed (value == "").
func (c Compose) withEnv(service, key, value string) (Compose, error) {
	out, err := clone(c)
	if err != nil {
		return nil, err
	}
	svc, err := out.service(service)
	if err != nil {
		return nil, err
	}
	switch env := svc["environment"].(type) {
	case []any:
		var list []any
		for _, e := range env {
			if kv, ok := e.(string); ok && strings.HasPrefix(kv, key+"=") {
				continue
			}
			list = append(list, e)
		}
		if value != "" {
			list = append(list, key+"="+value)
		}
		svc["environment"] = list
	default:
		m, _ := env.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		if value != "" {
			m[key] = value
		} else {
			delete(m, key)
		}
		svc["environment"] = m
	}
	return out, nil
}

// WithMaintenance returns a copy that starts the Hub read-only.
func (c Compose) WithMaintenance(service string) (Compose, error) {
	return c.withEnv(service, MaintenanceEnv, MaintenanceValue)
}

// Normalized returns the document with the image set to image and the
// maintenance variable removed: the two things the updater changes. A stored
// document that normalizes to the saved one was written by the updater.
func (c Compose) Normalized(service, image string) (Compose, error) {
	out, err := c.WithImage(service, image)
	if err != nil {
		return nil, err
	}
	if out.Env(service, MaintenanceEnv) == "" {
		return out, nil
	}
	return out.withEnv(service, MaintenanceEnv, "")
}
