//go:build race

package otel

// raceEnabled is true in a -race build (the detector slows exports ~10x).
const raceEnabled = true
