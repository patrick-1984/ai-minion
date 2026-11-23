//go:build !unix

package agent

import "os/exec"

func probeInGroup(*exec.Cmd)  {}
func endProbeGroup(*exec.Cmd) {}
