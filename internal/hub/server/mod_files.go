package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/files"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// filesModuleOrder places the file transfer module after the routing module
// (50), whose router it hooks, and the events module (55), whose session
// state tells whether a calling session is shared.
const filesModuleOrder = 57

func init() {
	DeclareLiveSetting(config.KeyFileTransferPolicy)
	DeclareLiveSetting(config.KeyFileTransferRetentionDays)
	registerFilesModule(DefaultRegistry)
}

// registerFilesModule registers the Hub's file transfer service (files.v1,
// WP7): the admission of file_pull and file_push behind the router
// (Router.SetFileAdmitter), the REST routing of file_list, file_pull and
// file_push, the Hub ops file_receipt and file_transfers_list, the public
// data-leg endpoint /files/v1/transfers/<id>/content for the web UI and REST
// callers, and the reconciliation loop.
func registerFilesModule(r *Registry) {
	if err := r.Register("files", filesModuleOrder, initFiles, DeclaresService[*files.Service]()); err != nil {
		panic(err)
	}
}

func initFiles(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	rt, err := Require[*workers.Router](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	reg := svc.Registry()
	legs := newWorkerLegs(reg)
	resolver := settings.Resolver()
	fc := files.Config{
		Store: st, Authz: az, Legs: legs, Logger: cfg.logger(),
		Conn: func(workerID string) []byte {
			on, ok := reg.Get(workerID)
			if !ok || on.Pending {
				return nil
			}
			return on.TranscriptHash
		},
		Policy: func() string {
			r, err := resolver.Resolve(config.KeyFileTransferPolicy)
			if err != nil || r.Value == "" {
				return ops.FileModeOff // fail closed
			}
			return r.Value
		},
		Retention: func() time.Duration {
			r, err := resolver.Resolve(config.KeyFileTransferRetentionDays)
			if err != nil {
				return 0
			}
			days, err := strconv.Atoi(r.Value)
			if err != nil || days <= 0 {
				return 0
			}
			return time.Duration(days) * 24 * time.Hour
		},
		KeyLive: func(ctx context.Context, keyID string) error {
			k, err := st.APIKeyByID(ctx, keyID)
			if errors.Is(err, store.ErrNotFound) {
				return proto.NewError(proto.CodeForbidden, "not allowed")
			}
			if err != nil {
				return err
			}
			now := time.Now().UnixMilli()
			if k.RevokedAt != 0 || (k.ExpiresAt != 0 && k.ExpiresAt <= now) {
				return proto.NewError(proto.CodeForbidden, "not allowed")
			}
			return nil
		},
		IsAdmin: func(ctx context.Context, p authz.Principal) (bool, error) {
			id := p.UserID
			if p.Kind == authz.KindActAs {
				return false, nil // acting as a user is that user
			}
			if id == "" {
				return false, nil
			}
			u, err := st.UserByID(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return err == nil && u.Active && u.Role == "admin", err
		},
	}
	if alog, err := Require[*audit.Log](env); err == nil {
		fc.Audit = alog
	}
	if hub, err := Require[*workers.EventHub](env); err == nil {
		fc.SessionShared = func(_ context.Context, workerID, session string) (bool, error) {
			ws, ok := hub.Sessions(workerID)
			if !ok {
				return false, nil
			}
			listed := slices.ContainsFunc(ws.Sessions, func(s workers.SessionState) bool { return s.Name == session })
			return listed && !hub.Fenced(workerID, session), nil
		}
	}
	fs, err := files.New(fc)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	rt.SetFileAdmitter(fs)
	if err := registerFileCalls(svc, fs); err != nil {
		return err
	}
	for _, name := range []string{ops.OpFileList, ops.OpFilePull, ops.OpFilePush} {
		if err := api.RegisterWorkerOp(handlers, name, rt); err != nil {
			return fmt.Errorf("files: %w", err)
		}
	}
	if err := api.HandleTyped(handlers, ops.OpFileReceipt, func(ctx context.Context, c *api.Call, args ops.FileReceiptArgs) (ops.FileReceiptResult, error) {
		return fs.ReceiptFor(ctx, c.Principal, args)
	}, api.WithResource(func(_ authz.Principal, args any) (authz.Resource, error) {
		// A receipt is about the target session of its transfer: the
		// caller that could admit the pull (an API key scoped to that
		// Worker) can send it. The receipt itself is bound to the
		// receiving identity and its leg token (Service.Receipt).
		a, ok := args.(*ops.FileReceiptArgs)
		if !ok || a.Validate() != nil {
			return nil, api.NewError(proto.CodeInvalidArgument, "invalid file_receipt arguments")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		row, err := st.FileTransfer(ctx, a.TransferID)
		if err != nil {
			return nil, api.NewError(proto.CodeNotFound, "not found")
		}
		return authz.SessionResource{WorkerID: row.TargetWorkerID, Name: row.TargetSession}, nil
	})); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	if err := api.HandleTyped(handlers, ops.OpFileTransfersList, func(ctx context.Context, c *api.Call, args ops.FileTransfersListArgs) (ops.FileTransfersListResult, error) {
		return fs.History(ctx, c.Principal, args)
	}, api.WithResource(func(p authz.Principal, _ any) (authz.Resource, error) {
		// The history is the caller's own (the store scopes it to the
		// viewer); worker_id is a filter, not the resource.
		if p.UserID == "" {
			return nil, api.NewError(proto.CodeForbidden, "not allowed")
		}
		return authz.UserResource{ID: p.UserID}, nil
	})); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	if mux, err := Require[*http.ServeMux](env); err == nil {
		if authn, err := Require[*api.Authenticator](env); err == nil {
			mux.Handle(proto.FilesLegPathPrefix, fs.Handler(func(r *http.Request) (authz.Principal, error) {
				id, err := authn.Authenticate(r)
				if err != nil {
					return authz.Principal{}, err
				}
				return id.Principal, nil
			}))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := false
	env.OnStart(func(context.Context) error {
		started = true
		go func() { defer close(done); fs.Run(ctx, 0) }()
		return nil
	})
	env.OnStop(func(stop context.Context) error {
		cancel()
		if !started {
			return nil
		}
		select {
		case <-done:
		case <-stop.Done():
		}
		return nil
	})
	return Provide(env, fs)
}

// legSession is the part of a Hub transport session that opens data legs;
// *transport.Session implements it.
type legSession interface {
	OpenLeg(ctx context.Context) (*transport.LegConn, error)
}

// workerLegs opens target legs on the Workers' current connections. It
// learns each connection from the registry when it is added; a leg is opened
// only on the connection with the transcript the transfer was admitted on,
// and only while the registry still has that connection.
type workerLegs struct {
	reg *workers.Registry
	mu  sync.Mutex
	by  map[string]workerLeg
}

type workerLeg struct {
	transcript []byte
	sess       legSession
}

func newWorkerLegs(reg *workers.Registry) *workerLegs {
	l := &workerLegs{reg: reg, by: map[string]workerLeg{}}
	reg.OnAdd(func(info workers.Online, sess workers.OpSession) {
		ls, ok := sess.(legSession)
		if !ok {
			return
		}
		l.mu.Lock()
		l.by[info.WorkerID] = workerLeg{transcript: slices.Clone(info.TranscriptHash), sess: ls}
		l.mu.Unlock()
	})
	return l
}

func (l *workerLegs) OpenLeg(ctx context.Context, workerID string, conn []byte) (*transport.LegConn, error) {
	on, ok := l.reg.Get(workerID)
	if !ok || on.Pending || !bytes.Equal(on.TranscriptHash, conn) || !on.HasFeature(proto.FeatureFilesV1) {
		return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker connection of the transfer is gone")
	}
	l.mu.Lock()
	w, ok := l.by[workerID]
	l.mu.Unlock()
	if !ok || !bytes.Equal(w.transcript, conn) {
		return nil, proto.NewError(proto.CodeWorkerOffline, "the Worker connection of the transfer is gone")
	}
	return w.sess.OpenLeg(ctx)
}
