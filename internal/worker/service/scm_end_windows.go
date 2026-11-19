//go:build windows

package service

import "golang.org/x/sys/windows/svc"

type endHandler struct{ fn func() }

// Execute reports the start, runs fn and returns: the manager records a
// stopped service with exit code 0, so the restart action does not apply.
func (h endHandler) Execute(_ []string, _ <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	h.fn()
	return false, 0
}

func endUnderSCM(fn func()) error { return svc.Run(WindowsServiceName, endHandler{fn}) }
