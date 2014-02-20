// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/loggo/loggo"
	"launchpad.net/gnuflag"
	"launchpad.net/tomb"

	"launchpad.net/juju-core/agent"
	"launchpad.net/juju-core/charm"
	"launchpad.net/juju-core/cmd"
	"launchpad.net/juju-core/container/kvm"
	"launchpad.net/juju-core/instance"
	"launchpad.net/juju-core/log/syslog"
	"launchpad.net/juju-core/names"
	"launchpad.net/juju-core/provider"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
	apiagent "launchpad.net/juju-core/state/api/agent"
	apimachiner "launchpad.net/juju-core/state/api/machiner"
	"launchpad.net/juju-core/state/api/params"
	apiprovisioner "launchpad.net/juju-core/state/api/provisioner"
	"launchpad.net/juju-core/state/apiserver"
	"launchpad.net/juju-core/upstart"
	"launchpad.net/juju-core/utils/voyeur"
	"launchpad.net/juju-core/worker"
	"launchpad.net/juju-core/worker/authenticationworker"
	"launchpad.net/juju-core/worker/charmrevisionworker"
	"launchpad.net/juju-core/worker/cleaner"
	"launchpad.net/juju-core/worker/deployer"
	"launchpad.net/juju-core/worker/firewaller"
	"launchpad.net/juju-core/worker/instancepoller"
	"launchpad.net/juju-core/worker/localstorage"
	workerlogger "launchpad.net/juju-core/worker/logger"
	"launchpad.net/juju-core/worker/machineenvironmentworker"
	"launchpad.net/juju-core/worker/machiner"
	"launchpad.net/juju-core/worker/minunitsworker"
	"launchpad.net/juju-core/worker/provisioner"
	"launchpad.net/juju-core/worker/resumer"
	"launchpad.net/juju-core/worker/terminationworker"
	"launchpad.net/juju-core/worker/upgrader"
)

var logger = loggo.GetLogger("juju.cmd.jujud")

var newRunner = func(isFatal func(error) bool, moreImportant func(e0, e1 error) bool) worker.Runner {
	return worker.NewRunner(isFatal, moreImportant)
}

const bootstrapMachineId = "0"

var retryDelay = 3 * time.Second

var jujuRun = "/usr/local/bin/juju-run"

// MachineAgent is a cmd.Command responsible for running a machine agent.
type MachineAgent struct {
	cmd.CommandBase
	tomb      tomb.Tomb
	Conf      AgentConf
	MachineId string
	runner    worker.Runner
	configVal *voyeur.Value
}

func NewMachineAgent() *MachineAgent {
	return &MachineAgent{configVal: voyeur.NewValue(nil)}
}

// Info returns usage information for the command.
func (a *MachineAgent) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "machine",
		Purpose: "run a juju machine agent",
	}
}

func (a *MachineAgent) SetFlags(f *gnuflag.FlagSet) {
	a.Conf.addFlags(f)
	f.StringVar(&a.MachineId, "machine-id", "", "id of the machine to run")
}

// Init initializes the command for running.
func (a *MachineAgent) Init(args []string) error {
	if !names.IsMachine(a.MachineId) {
		return fmt.Errorf("--machine-id option must be set, and expects a non-negative integer")
	}
	if err := a.Conf.checkArgs(args); err != nil {
		return err
	}
	a.runner = newRunner(isFatal, moreImportant)
	return nil
}

// Wait waits for the machine agent to finish.
func (a *MachineAgent) Wait() error {
	return a.tomb.Wait()
}

// Stop stops the machine agent.
func (a *MachineAgent) Stop() error {
	a.runner.Kill()
	return a.tomb.Wait()
}

// Run runs a machine agent.
func (a *MachineAgent) Run(_ *cmd.Context) error {
	// Due to changes in the logging, and needing to care about old
	// environments that have been upgraded, we need to explicitly remove the
	// file writer if one has been added, otherwise we will get duplicate
	// lines of all logging in the log file.
	loggo.RemoveWriter("logfile")
	defer a.tomb.Done()
	logger.Infof("machine agent %v start", a.Tag())
	if err := a.Conf.read(a.Tag()); err != nil {
		return err
	}
	a.setAgentConfig(a.Conf.config)
	charm.CacheDir = filepath.Join(a.Conf.dataDir, "charmcache")
	if err := a.initAgent(); err != nil {
		return err
	}
	a.runner.StartWorker("api", a.APIWorker)
	a.runner.StartWorker("statestarter", a.newStateStarterWorker)
	a.runner.StartWorker("termination", func() (worker.Worker, error) {
		return terminationworker.NewWorker(), nil
	})
	err := a.runner.Wait()
	if err == worker.ErrTerminateAgent {
		err = a.uninstallAgent()
	}
	err = agentDone(err)
	a.tomb.Kill(err)
	return err
}

// newStateStarterWorker wraps stateStarter in a simple worker for use in
// a.runner.StartWorker.
func (a *MachineAgent) newStateStarterWorker() (worker.Worker, error) {
	return worker.NewSimpleWorker(a.stateStarter), nil
}

// stateStarter watches for changes to the agent configuration, and starts or
// stops the state worker as appropriate.  It will stop working as soon as
// stopch is closed.
func (a *MachineAgent) stateStarter(stopch <-chan struct{}) error {
	confWatch := a.configVal.Watch()
	defer confWatch.Close()
	watchCh := make(chan agent.Config)
	go func() {
		for confWatch.Next() {
			v, _ := confWatch.Value().(agent.Config)
			watchCh <- v
		}
	}()
	for {
		select {
		case conf := <-watchCh:
			// N.B. StartWorker and StopWorker are idempotent.
			if conf.StateManager() {
				a.runner.StartWorker("state", func() (worker.Worker, error) {
					return a.StateWorker(conf)
				})
			} else {
				a.runner.StopWorker("state")
			}
		case <-stopch:
			return nil
		}
	}
}

// APIWorker returns a Worker that connects to the API and starts any
// workers that need an API connection.
// It is also responsible for maintaining the agent config
// by saving it to disk and calling setAgentConfig.
func (a *MachineAgent) APIWorker() (worker.Worker, error) {
	agentConfig := a.Conf.config
	st, entity, err := openAPIState(agentConfig, a)
	if err != nil {
		return nil, err
	}
	reportOpenedAPI(st)
	for _, job := range entity.Jobs() {
		if job.NeedsState() {
			a.setAgentConfig(agentConfig)
			break
		}
	}
	runner := newRunner(connectionIsFatal(st), moreImportant)
	runner.StartWorker("machiner", func() (worker.Worker, error) {
		return machiner.NewMachiner(st.Machiner(), agentConfig), nil
	})
	runner.StartWorker("upgrader", func() (worker.Worker, error) {
		return upgrader.NewUpgrader(st.Upgrader(), agentConfig), nil
	})
	runner.StartWorker("logger", func() (worker.Worker, error) {
		return workerlogger.NewLogger(st.Logger(), agentConfig), nil
	})
	runner.StartWorker("machineenvironmentworker", func() (worker.Worker, error) {
		return machineenvironmentworker.NewMachineEnvironmentWorker(st.Environment(), agentConfig), nil
	})
	// runner.StartWorker("configwatcher", func() (worker.Worker, error) {
	// 	return a.newConfigWatcher(st.Machiner())
	// })

	// If not a local provider bootstrap machine, start the worker to manage SSH keys.
	providerType := agentConfig.Value(agent.ProviderType)
	if providerType != provider.Local || a.MachineId != bootstrapMachineId {
		runner.StartWorker("authenticationworker", func() (worker.Worker, error) {
			return authenticationworker.NewWorker(st.KeyUpdater(), agentConfig), nil
		})
	}

	// Perform the operations needed to set up hosting for containers.
	if err := a.setupContainerSupport(runner, st, entity); err != nil {
		return nil, fmt.Errorf("setting up container support: %v", err)
	}
	for _, job := range entity.Jobs() {
		switch job {
		case params.JobHostUnits:
			runner.StartWorker("deployer", func() (worker.Worker, error) {
				apiDeployer := st.Deployer()
				context := newDeployContext(apiDeployer, agentConfig)
				return deployer.NewDeployer(apiDeployer, context), nil
			})
		case params.JobManageEnviron:
			runner.StartWorker("environ-provisioner", func() (worker.Worker, error) {
				return provisioner.NewEnvironProvisioner(st.Provisioner(), agentConfig), nil
			})
			// TODO(axw) 2013-09-24 bug #1229506
			// Make another job to enable the firewaller. Not all environments
			// are capable of managing ports centrally.
			runner.StartWorker("firewaller", func() (worker.Worker, error) {
				return firewaller.NewFirewaller(st.Firewaller())
			})
			runner.StartWorker("charm-revision-updater", func() (worker.Worker, error) {
				return charmrevisionworker.NewRevisionUpdateWorker(st.CharmRevisionUpdater()), nil
			})
		case params.JobManageState:
			// Legacy environments may set this, but we ignore it.
		default:
			// TODO(dimitern): Once all workers moved over to using
			// the API, report "unknown job type" here.
		}
	}
	return newCloseWorker(runner, st), nil // Note: a worker.Runner is itself a worker.Worker.
}

func (a *MachineAgent) newConfigWatcher(st *apimachiner.State) (worker.Worker, error) {
	// TODO: (Nate) :
	//	watch machine jobs
	//  watch state addresses
	//  watch API addresses
	//	when any of them change, change the agent config, save it and call a.setAgentConfig
	return nil, nil
}

// setupContainerSupport determines what containers can be run on this machine and
// initialises suitable infrastructure to support such containers.
func (a *MachineAgent) setupContainerSupport(runner worker.Runner, st *api.State, entity *apiagent.Entity) error {
	var supportedContainers []instance.ContainerType
	// We don't yet support nested lxc containers but anything else can run an LXC container.
	if entity.ContainerType() != instance.LXC {
		supportedContainers = append(supportedContainers, instance.LXC)
	}
	supportsKvm, err := kvm.IsKVMSupported()
	if err != nil {
		logger.Warningf("determining kvm support: %v\nno kvm containers possible", err)
	}
	if err == nil && supportsKvm {
		supportedContainers = append(supportedContainers, instance.KVM)
	}
	return a.updateSupportedContainers(runner, st, entity.Tag(), supportedContainers)
}

// updateSupportedContainers records in state that a machine can run the specified containers.
// It starts a watcher and when a container of a given type is first added to the machine,
// the watcher is killed, the machine is set up to be able to start containers of the given type,
// and a suitable provisioner is started.
func (a *MachineAgent) updateSupportedContainers(runner worker.Runner, st *api.State,
	tag string, containers []instance.ContainerType) error {

	var machine *apiprovisioner.Machine
	var err error
	pr := st.Provisioner()
	if machine, err = pr.Machine(tag); err != nil {
		return fmt.Errorf("%s is not in state: %v", tag, err)
	}
	if len(containers) == 0 {
		if err := machine.SupportsNoContainers(); err != nil {
			return fmt.Errorf("clearing supported containers for %s: %v", tag, err)
		}
		return nil
	}
	if err := machine.SetSupportedContainers(containers...); err != nil {
		return fmt.Errorf("setting supported containers for %s: %v", tag, err)
	}
	// Start the watcher to fire when a container is first requested on the machine.
	watcherName := fmt.Sprintf("%s-container-watcher", machine.Id())
	handler := provisioner.NewContainerSetupHandler(runner, watcherName, containers, machine, pr, a.Conf.config)
	runner.StartWorker(watcherName, func() (worker.Worker, error) {
		return worker.NewStringsWorker(handler), nil
	})
	return nil
}

// StateJobs returns a worker running all the workers that require
// a *state.State connection.
func (a *MachineAgent) StateWorker(agentConfig agent.Config) (worker.Worker, error) {
	st, entity, err := openState(agentConfig, a)
	if err != nil {
		return nil, err
	}
	reportOpenedState(st)
	m := entity.(*state.Machine)

	runner := newRunner(connectionIsFatal(st), moreImportant)

	// Take advantage of special knowledge here in that we will only ever want
	// the storage provider on one machine, and that is the "bootstrap" node.
	providerType := agentConfig.Value(agent.ProviderType)
	if (providerType == provider.Local || provider.IsManual(providerType)) && m.Id() == bootstrapMachineId {
		runner.StartWorker("local-storage", func() (worker.Worker, error) {
			// TODO(axw) 2013-09-24 bug #1229507
			// Make another job to enable storage.
			// There's nothing special about this.
			return localstorage.NewWorker(agentConfig), nil
		})
	}
	for _, job := range m.Jobs() {
		switch job {
		case state.JobHostUnits:
			// Implemented in APIWorker.
		case state.JobManageEnviron:
			runner.StartWorker("instancepoller", func() (worker.Worker, error) {
				return instancepoller.NewWorker(st), nil
			})
			runner.StartWorker("apiserver", func() (worker.Worker, error) {
				// If the configuration does not have the required information,
				// it is currently not a recoverable error, so we kill the whole
				// agent, potentially enabling human intervention to fix
				// the agent's configuration file. In the future, we may retrieve
				// the state server certificate and key from the state, and
				// this should then change.
				port, cert, key := a.Conf.config.APIServerDetails()
				if len(cert) == 0 || len(key) == 0 {
					return nil, &fatalError{"configuration does not have state server cert/key"}
				}
				dataDir := a.Conf.config.DataDir()
				return apiserver.NewServer(st, fmt.Sprintf(":%d", port), cert, key, dataDir)
			})
			runner.StartWorker("cleaner", func() (worker.Worker, error) {
				return cleaner.NewCleaner(st), nil
			})
			runner.StartWorker("resumer", func() (worker.Worker, error) {
				// The action of resumer is so subtle that it is not tested,
				// because we can't figure out how to do so without brutalising
				// the transaction log.
				return resumer.NewResumer(st), nil
			})
			runner.StartWorker("minunitsworker", func() (worker.Worker, error) {
				return minunitsworker.NewMinUnitsWorker(st), nil
			})
		case state.JobManageState:
			// Legacy environments may set this, but we ignore it.
		default:
			logger.Warningf("ignoring unknown job %q", job)
		}
	}
	return newCloseWorker(runner, st), nil
}

func (a *MachineAgent) Entity(st *state.State) (AgentState, error) {
	m, err := st.Machine(a.MachineId)
	if err != nil {
		return nil, err
	}
	// Check the machine nonce as provisioned matches the agent.Conf value.
	if !m.CheckProvisioned(a.Conf.config.Nonce()) {
		// The agent is running on a different machine to the one it
		// should be according to state. It must stop immediately.
		logger.Errorf("running machine %v agent on inappropriate instance", m)
		return nil, worker.ErrTerminateAgent
	}
	return m, nil
}

func (a *MachineAgent) Tag() string {
	return names.MachineTag(a.MachineId)
}

func (a *MachineAgent) initAgent() error {
	if err := os.Remove(jujuRun); err != nil && !os.IsNotExist(err) {
		return err
	}
	jujud := filepath.Join(a.Conf.dataDir, "tools", a.Tag(), "jujud")
	return os.Symlink(jujud, jujuRun)
}

func (a *MachineAgent) uninstallAgent() error {
	var errors []error
	agentServiceName := a.Conf.config.Value(agent.AgentServiceName)
	if agentServiceName == "" {
		// For backwards compatibility, handle lack of AgentServiceName.
		agentServiceName = os.Getenv("UPSTART_JOB")
	}
	if agentServiceName != "" {
		if err := upstart.NewService(agentServiceName).Remove(); err != nil {
			errors = append(errors, fmt.Errorf("cannot remove service %q: %v", agentServiceName, err))
		}
	}
	// Remove the rsyslog conf file and restart rsyslogd.
	if rsyslogConfPath := a.Conf.config.Value(agent.RsyslogConfPath); rsyslogConfPath != "" {
		if err := os.Remove(rsyslogConfPath); err != nil {
			errors = append(errors, err)
		}
		if err := syslog.Restart(); err != nil {
			errors = append(errors, err)
		}
	}
	// Remove the juju-run symlink.
	if err := os.Remove(jujuRun); err != nil && !os.IsNotExist(err) {
		errors = append(errors, err)
	}
	// The machine agent may terminate without knowing its jobs,
	// for example if the machine's entry in state was removed.
	// Thus, we do not rely on jobs here, and instead just check
	// if the upstart config exists.
	mongoServiceName := a.Conf.config.Value(agent.MongoServiceName)
	if mongoServiceName != "" {
		if err := upstart.NewService(mongoServiceName).StopAndRemove(); err != nil {
			errors = append(errors, fmt.Errorf("cannot stop/remove service %q: %v", mongoServiceName, err))
		}
	}
	if err := os.RemoveAll(a.Conf.dataDir); err != nil {
		errors = append(errors, err)
	}
	if len(errors) == 0 {
		return nil
	}
	return fmt.Errorf("uninstall failed: %v", errors)
}

// Below pieces are used for testing,to give us access to the *State opened
// by the agent, and allow us to trigger syncs without waiting 5s for them
// to happen automatically.

var stateReporter chan<- *state.State

func reportOpenedState(st *state.State) {
	select {
	case stateReporter <- st:
	default:
	}
}

func sendOpenedStates(dst chan<- *state.State) (undo func()) {
	var original chan<- *state.State
	original, stateReporter = stateReporter, dst
	return func() { stateReporter = original }
}

var apiReporter chan<- *api.State

func reportOpenedAPI(st *api.State) {
	select {
	case apiReporter <- st:
	default:
	}
}
func sendOpenedAPIs(dst chan<- *api.State) (undo func()) {
	var original chan<- *api.State
	original, apiReporter = apiReporter, dst
	return func() { apiReporter = original }
}

func (a *MachineAgent) setAgentConfig(conf agent.Config) {
	a.configVal.Set(conf.Clone())
}

func (a *MachineAgent) agentConfig() agent.Config {
	val, _ := a.configVal.Get()
	conf, _ := val.(agent.Config)
	return conf
}

// ensureMongoServer ensures that the correct mongo upstart script is installed
// and running.
//
// This method will remove old versions of the mongo upstart script as necessary
// before installing the new version.
func (a *MachineAgent) ensureMongoServer() error {
	service := a.mongoService()
	if service.Installed() {
		return nil
	}

	if err := removeOldMongoServices(); err != nil {
		return err
	}

	journalDir := filepath.Join(a.mongoDir(), "journal")

	if err := os.MkdirAll(journalDir, 0700); err != nil {
		logger.Errorf("failed to make mongo journal dir %s: %v", journalDir, err)
		return err
	}

	// manually create the prealloc files, since otherwise they get created as 100M files.
	zeroes := make([]byte, 1024*1024)
	for x := 0; x < 3; x++ {
		name := fmt.Sprintf("prealloc.%d", x)
		filename := filepath.Join(journalDir, name)
		if err := ioutil.WriteFile(filename, zeroes, 700); err != nil {
			logger.Errorf("failed to make write mongo prealloc file: %v", journalDir, err)
			return err
		}
	}

	if err := service.Install(); err != nil {
		logger.Errorf("Failed to install mongo service %q: %v", service.Name, err)
		return err
	}
	return service.Start()
}

// mongoDir returns the directory that mongo should use to store its data.
func (a *MachineAgent) mongoDir() string {
	return a.Conf.dataDir
}

// mongoSvcConfig returns the upstart configuration object for mongo.
func (a *MachineAgent) mongoService() *upstart.Conf {
	return upstart.MongoUpstartService(
		mongoServiceName,
		a.mongoDir(),
		a.Conf.config.StatePort())
}

// removeOldMongoServices looks for any old juju mongo upstart scripts and
// removes them.
func removeOldMongoServices() error {
	old := upstart.NewService(oldMongoServiceName)
	if err := old.StopAndRemove(); err != nil {
		logger.Errorf("Failed to remove old mongo upstart service %q: %v", old.Name, err)
		return err
	}

	// the new formatting for the script name started at version 2
	for x := 2; x < upstart.MongoScriptVersion; x++ {
		old := upstart.NewService(fmt.Sprintf(mongoSvcFmt, x))
		if err := old.StopAndRemove(); err != nil {
			logger.Errorf("Failed to remove old mongo upstart service %q: %v", old.Name, err)
			return err
		}
	}
	return nil
}
