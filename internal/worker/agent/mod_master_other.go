//go:build !linux || android

package agent

import "context"

func startMaster(context.Context, *Env) (StopFunc, error) { return nil, nil }
