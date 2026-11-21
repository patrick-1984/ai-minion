package truenas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	b, err := json.Marshal(map[string]any(c))
	if err != nil {
		return nil, err
	}
	var out Compose
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	svc, err := out.service(service)
	if err != nil {
		return nil, err
	}
	svc["image"] = ref
	return out, nil
}
