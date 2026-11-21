package localapi

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	MasterRegister = "register"
	MasterStatus   = "status"
	MasterCatalog  = "catalog"
	MasterCall     = "call"
)

// MasterRequest is used on the dedicated local master socket. It contains no
// lease handle or claimed PID. The server identifies the peer from the socket.
type MasterRequest struct {
	Kind       string          `json:"kind"`
	Executable string          `json:"executable,omitempty"`
	Generation uint64          `json:"generation,omitempty"`
	Report     string          `json:"report,omitempty"`
	Operation  string          `json:"operation,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
}

type MasterResponse struct {
	Generation uint64            `json:"generation,omitempty"`
	Result     json.RawMessage   `json:"result,omitempty"`
	Error      *proto.ErrorFrame `json:"error,omitempty"`
}

// MasterSocketPaths uses the Worker runtime directory, including the service
// directory fallback, and never names the general control socket.
func MasterSocketPaths(env config.PathEnv) ([]string, error) {
	if env.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	paths, err := SocketCandidates(env)
	for i := range paths {
		paths[i] = filepath.Join(filepath.Dir(paths[i]), "master.sock")
	}
	return paths, err
}

// CallMaster sends one request and reads one reply. Once connected it never
// retries, even if the response to a write cannot be read.
func CallMaster(ctx context.Context, path string, req MasterRequest) (MasterResponse, error) {
	if err := proto.WriteFrame(io.Discard, proto.MasterFrameLimit, req); err != nil {
		return MasterResponse{}, err
	}
	if err := VerifySocket(path); err != nil {
		return MasterResponse{}, err
	}
	conn, err := DialContext(ctx, path)
	if err != nil {
		return MasterResponse{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(DefaultRequestTimeout))
	if req.Kind == MasterCall {
		_ = conn.SetDeadline(time.Now().Add(35 * time.Second))
	}
	lost := func(err error) (MasterResponse, error) {
		if req.Kind != MasterCall || req.Operation != ops.OpSendText {
			return MasterResponse{}, err
		}
		b, _ := json.Marshal(ops.WriteResult{
			Outcome: ops.OutcomeUnknown, Reason: string(proto.CodeWorkerOffline),
			Warnings: []ops.Warning{{Code: "local_master_call_lost",
				Message: "the local master call ended before its answer. Check the target before retrying with the same request ID"}},
		})
		return MasterResponse{Result: b}, nil
	}
	if err := proto.WriteFrame(conn, proto.MasterFrameLimit, req); err != nil {
		return lost(err)
	}
	body, err := proto.ReadFrame(conn, proto.StreamFrameLimit)
	if err != nil {
		return lost(err)
	}
	var res MasterResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return lost(err)
	}
	if res.Error != nil {
		return MasterResponse{}, res.Error.Err()
	}
	return res, nil
}
