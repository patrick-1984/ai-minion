package agent

import "context"

const (
	ModuleMaster = "master"
	OrderMaster  = 190
)

func init() { RegisterModule(ModuleMaster, OrderMaster, startMaster) }

type masterIntegration struct {
	serve LinkHandler
	// files sends a file op of a calling session on the master lane, when
	// the session holds a verified lease (master_files_linux.go); nil where
	// there is no master lane.
	files masterFiles
	// apps sends app_install and app_install_status of a calling session on
	// the master lane under the same conditions (master_files_linux.go).
	apps masterFiles
}

// masterFiles sends the file op op of the local caller pid in session on the
// master lane and decodes the result into out. handled is false when the
// lane does not serve the caller; the caller then uses the ordinary call
// stream.
type masterFiles func(ctx context.Context, session string, pid int, op string, args any, out any) (handled bool, err error)

func masterFeature(env *Env) *masterIntegration {
	integration, _ := lookupService[*masterIntegration](env.rt)
	return integration
}
