//go:build linux && !android

package localapi

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestMasterLocalFramingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		body, err := proto.ReadFrame(conn, proto.MasterFrameLimit)
		if err != nil {
			done <- err
			return
		}
		var request MasterRequest
		if err := proto.DecodeMasterJSON(body, &request); err != nil {
			done <- err
			return
		}
		if request.Type != TypeMasterLocalRequest || request.Kind != MasterStatus {
			done <- fmt.Errorf("wrong socket frame type %q or action %q", request.Type, request.Kind)
			return
		}
		done <- proto.WriteFrame(conn, proto.StreamFrameLimit, MasterResponse{Type: TypeMasterLocalResponse, Generation: 7})
	}()
	result, err := CallMaster(t.Context(), path, MasterRequest{Kind: MasterStatus})
	if err != nil || result.Generation != 7 {
		t.Fatalf("real local response = %+v, error = %v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
