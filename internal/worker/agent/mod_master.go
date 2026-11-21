package agent

const (
	ModuleMaster = "master"
	OrderMaster  = 190
)

func init() { RegisterModule(ModuleMaster, OrderMaster, startMaster) }

type masterIntegration struct {
	serve LinkHandler
}

func masterFeature(env *Env) *masterIntegration {
	integration, _ := lookupService[*masterIntegration](env.rt)
	return integration
}
