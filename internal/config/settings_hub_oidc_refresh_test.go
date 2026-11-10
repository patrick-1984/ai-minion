package config

import (
	"testing"
	"time"
)

func TestHubOIDCRefreshIntervalDeclared(t *testing.T) {
	s, ok := Default.Lookup(KeyOIDCRefreshInterval)
	if !ok {
		t.Fatalf("setting %s not declared", KeyOIDCRefreshInterval)
	}
	if KeyOIDCRefreshInterval != "oidc_refresh_interval" || s.Scope != ScopeHub || s.EnvName() != "MINION_OIDC_REFRESH_INTERVAL" ||
		s.HasFlag() || s.Secret || s.Default != "24h" || s.Help == "" {
		t.Errorf("setting = %+v (env %s)", s, s.EnvName())
	}
	if DefaultOIDCRefreshInterval != 24*time.Hour || MinOIDCRefreshInterval != time.Minute || MaxOIDCRefreshInterval != 720*time.Hour {
		t.Errorf("bounds %v %v %v", DefaultOIDCRefreshInterval, MinOIDCRefreshInterval, MaxOIDCRefreshInterval)
	}
}

func TestHubOIDCRefreshIntervalResolves(t *testing.T) {
	res, err := hubEnv(nil).Resolve(KeyOIDCRefreshInterval)
	if err != nil || res.Value != "24h" || res.Source != SourceDefault {
		t.Fatalf("default = %+v, %v", res, err)
	}
	res, err = hubEnv(map[string]string{"MINION_OIDC_REFRESH_INTERVAL": "6h"}).Resolve(KeyOIDCRefreshInterval)
	if err != nil || res.Value != "6h" || res.Source != SourceEnv {
		t.Fatalf("env = %+v, %v", res, err)
	}
}
