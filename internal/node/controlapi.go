package nexnode

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/pkg/errors"
	agentapi "github.com/synadia-io/nex/internal/agent-api"
	controlapi "github.com/synadia-io/nex/internal/control-api"
)

// The API listener is the command and control interface for the node server
type ApiListener struct {
	mgr    *MachineManager
	log    *slog.Logger
	nodeId string
	start  time.Time
	xk     nkeys.KeyPair
	config *NodeConfiguration
}

func NewApiListener(log *slog.Logger, mgr *MachineManager, config *NodeConfiguration) *ApiListener {
	efftags := config.Tags
	efftags[controlapi.TagOS] = runtime.GOOS
	efftags[controlapi.TagArch] = runtime.GOARCH
	efftags[controlapi.TagCPUs] = strconv.FormatInt(int64(runtime.NumCPU()), 10)

	kp, err := nkeys.CreateCurveKeys()
	if err != nil {
		log.Error("Failed to create x509 curve key", slog.Any("err", err))
		return nil
	}
	xkPub, err := kp.PublicKey()
	if err != nil {
		log.Error("Failed to get public key from x509 curve key", slog.Any("err", err))
		return nil
	}

	log.Info("Use this key as the recipient for encrypted run requests", slog.String("public_xkey", xkPub))

	return &ApiListener{
		mgr:    mgr,
		log:    log,
		nodeId: mgr.publicKey,
		xk:     kp,
		start:  time.Now().UTC(),
		config: config,
	}
}

func (api *ApiListener) PublicKey() string {
	return api.mgr.publicKey
}

func (api *ApiListener) Start() error {
	_, err := api.mgr.nc.Subscribe(controlapi.APIPrefix+".PING", api.handlePing)
	if err != nil {
		api.log.Error("Failed to subscribe to ping subject", slog.Any("err", err), slog.String("id", api.nodeId))
	}

	_, err = api.mgr.nc.Subscribe(controlapi.APIPrefix+".PING."+api.nodeId, api.handlePing)
	if err != nil {
		api.log.Error("Failed to subscribe to node-specific ping subject", slog.Any("err", err), slog.String("id", api.nodeId))
	}

	// Namespaced subscriptions, the * below is for the namespace
	_, err = api.mgr.nc.Subscribe(controlapi.APIPrefix+".INFO.*."+api.nodeId, api.handleInfo)
	if err != nil {
		api.log.Error("Failed to subscribe to info subject", slog.Any("err", err), slog.String("id", api.nodeId))
	}

	_, err = api.mgr.nc.Subscribe(controlapi.APIPrefix+".DEPLOY.*."+api.nodeId, api.handleDeploy)
	if err != nil {
		api.log.Error("Failed to subscribe to run subject", slog.Any("err", err), slog.String("id", api.nodeId))
	}

	_, err = api.mgr.nc.Subscribe(controlapi.APIPrefix+".STOP.*."+api.nodeId, api.handleStop)
	if err != nil {
		api.log.Error("Failed to subscribe to stop subject", slog.Any("err", err), slog.String("id", api.nodeId))
	}

	api.log.Info("NATS execution engine awaiting commands", slog.String("id", api.nodeId), slog.String("version", VERSION))
	return nil
}

func (api *ApiListener) handleStop(m *nats.Msg) {
	namespace, err := extractNamespace(m.Subject)
	if err != nil {
		api.log.Error("Invalid subject for workload stop", slog.Any("err", err))
		respondFail(controlapi.StopResponseType, m, "Invalid subject for workload stop")
		return
	}

	var request controlapi.StopRequest
	err = json.Unmarshal(m.Data, &request)
	if err != nil {
		api.log.Error("Failed to deserialize stop request", slog.Any("err", err))
		respondFail(controlapi.StopResponseType, m, fmt.Sprintf("Unable to deserialize stop request: %s", err))
		return
	}

	vm := api.mgr.LookupMachine(request.WorkloadId)
	if vm == nil {
		api.log.Error("Stop request: no such workload", slog.String("vmid", request.WorkloadId))
		respondFail(controlapi.StopResponseType, m, "No such workload")
		return
	}

	if vm.namespace != namespace {
		api.log.Error("Namespace mismatch on workload stop request",
			slog.String("namespace", vm.namespace),
			slog.String("targetnamespace", namespace),
		)

		respondFail(controlapi.StopResponseType, m, "No such workload") // do not expose ID existence to avoid existence probes
		return
	}

	err = request.Validate(&vm.deployRequest.DecodedClaims)
	if err != nil {
		api.log.Error("Failed to validate stop request", slog.Any("err", err))
		respondFail(controlapi.StopResponseType, m, fmt.Sprintf("Invalid stop request: %s", err))
		return
	}

	err = api.mgr.StopMachine(request.WorkloadId)
	if err != nil {
		api.log.Error("Failed to stop workload", slog.Any("err", err))
		respondFail(controlapi.StopResponseType, m, fmt.Sprintf("Failed to stop workload: %s", err))
	}

	res := controlapi.NewEnvelope(controlapi.StopResponseType, controlapi.StopResponse{
		Stopped:   true,
		Name:      vm.deployRequest.DecodedClaims.Subject,
		Issuer:    vm.deployRequest.DecodedClaims.Issuer,
		MachineId: vm.vmmID,
	}, nil)
	raw, err := json.Marshal(res)
	if err != nil {
		api.log.Error("Failed to marshal run response", slog.Any("err", err))
	} else {
		_ = m.Respond(raw)
	}
}

func (api *ApiListener) handleDeploy(m *nats.Msg) {
	namespace, err := extractNamespace(m.Subject)
	if err != nil {
		api.log.Error("Invalid subject for workload deployment", slog.Any("err", err))
		respondFail(controlapi.RunResponseType, m, "Invalid subject for workload deployment")
		return
	}

	var request controlapi.DeployRequest
	err = json.Unmarshal(m.Data, &request)
	if err != nil {
		api.log.Error("Failed to deserialize deploy request", slog.Any("err", err))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("Unable to deserialize deploy request: %s", err))
		return
	}

	if !slices.Contains(api.config.WorkloadTypes, *request.WorkloadType) {
		api.log.Error("This node does not support the given workload type", slog.String("workload_type", *request.WorkloadType))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("Unsupported workload type on this node: %s", *request.WorkloadType))
		return
	}

	if len(request.TriggerSubjects) > 0 && (!strings.EqualFold(*request.WorkloadType, "v8") &&
		!strings.EqualFold(*request.WorkloadType, "wasm")) { // FIXME -- workload type comparison
		api.log.Error("Workload type does not support trigger subject registration", slog.String("trigger_subjects", *request.WorkloadType))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("Unsupported workload type for trigger subject registration: %s", *request.WorkloadType))
		return
	}

	decodedClaims, err := request.Validate(api.xk)
	if err != nil {
		api.log.Error("Invalid deploy request", slog.Any("err", err))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("Invalid deploy request: %s", err))
		return
	}

	request.DecodedClaims = *decodedClaims
	if !validateIssuer(request.DecodedClaims.Issuer, api.mgr.config.ValidIssuers) {
		err := fmt.Errorf("invalid workload issuer: %s", request.DecodedClaims.Issuer)
		api.log.Error("Workload validation failed", slog.Any("err", err))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("%s", err))
	}

	numBytes, workloadHash, err := api.mgr.CacheWorkload(&request)
	if err != nil {
		api.log.Error("Failed to cache workload bytes", slog.Any("err", err))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("Failed to cache workload bytes: %s", err))
		return
	}

	runningVM := <-api.mgr.warmVMs
	workloadName := request.DecodedClaims.Subject

	api.log.
		Info("Submitting workload to VM",
			slog.String("vmid", runningVM.vmmID),
			slog.String("namespace", namespace),
			slog.String("workload", workloadName),
			slog.Uint64("workload_size", numBytes),
			slog.String("workload_sha256", *workloadHash),
			slog.String("type", *request.WorkloadType),
		)

	err = api.mgr.DeployWorkload(runningVM, &agentapi.DeployRequest{
		Argv:            request.Argv,
		DecodedClaims:   request.DecodedClaims,
		Description:     request.Description,
		Environment:     request.WorkloadEnvironment,
		Hash:            *workloadHash,
		Namespace:       &namespace,
		TotalBytes:      int64(numBytes),
		TriggerSubjects: request.TriggerSubjects,
		WorkloadName:    &workloadName,
		WorkloadType:    request.WorkloadType, // FIXME-- audit all types for string -> *string, and validate...
	})

	if err != nil {
		api.log.Error("Failed to deploy workload in VM", slog.Any("err", err))
		respondFail(controlapi.RunResponseType, m, fmt.Sprintf("Unable to deploy workload: %s", err))
		return
	}
	api.log.Info("Workload deployed", slog.String("workload", workloadName), slog.String("vmid", runningVM.vmmID))

	res := controlapi.NewEnvelope(controlapi.RunResponseType, controlapi.RunResponse{
		Started:   true,
		Name:      workloadName,
		Issuer:    runningVM.deployRequest.DecodedClaims.Issuer,
		MachineId: runningVM.vmmID,
	}, nil)

	raw, err := json.Marshal(res)
	if err != nil {
		api.log.Error("Failed to marshal deploy response", slog.Any("err", err))
	} else {
		_ = m.Respond(raw)
	}
}

func (api *ApiListener) handlePing(m *nats.Msg) {
	now := time.Now().UTC()
	res := controlapi.NewEnvelope(controlapi.PingResponseType, controlapi.PingResponse{
		NodeId:          api.nodeId,
		Version:         Version(),
		Uptime:          myUptime(now.Sub(api.start)),
		RunningMachines: len(api.mgr.allVMs),
		Tags:            api.config.Tags,
	}, nil)

	raw, err := json.Marshal(res)
	if err != nil {
		api.log.Error("Failed to marshal ping response", slog.Any("err", err))
	} else {
		_ = m.Respond(raw)
	}
}

func (api *ApiListener) handleInfo(m *nats.Msg) {
	namespace, err := extractNamespace(m.Subject)
	if err != nil {
		api.log.Error("Failed to extract namespace for info request", slog.Any("err", err))
		respondFail(controlapi.InfoResponseType, m, "Failed to extract namespace for info request")
		return
	}

	pubX, _ := api.xk.PublicKey()
	now := time.Now().UTC()
	stats, _ := ReadMemoryStats()
	res := controlapi.NewEnvelope(controlapi.InfoResponseType, controlapi.InfoResponse{
		Version:                VERSION,
		PublicXKey:             pubX,
		Uptime:                 myUptime(now.Sub(api.start)),
		Tags:                   api.config.Tags,
		SupportedWorkloadTypes: api.config.WorkloadTypes,
		Machines:               summarizeMachines(&api.mgr.allVMs, namespace),
		Memory:                 stats,
	}, nil)

	raw, err := json.Marshal(res)
	if err != nil {
		api.log.Error("Failed to marshal ping response", slog.Any("err", err))
	} else {
		_ = m.Respond(raw)
	}
}

func summarizeMachines(vms *map[string]*runningFirecracker, namespace string) []controlapi.MachineSummary {
	machines := make([]controlapi.MachineSummary, 0)
	now := time.Now().UTC()
	for _, v := range *vms {
		if v.namespace == namespace {
			var desc string
			if v.deployRequest.Description != nil {
				desc = *v.deployRequest.Description // FIXME-- audit controlapi.WorkloadSummary
			}

			var workloadType string
			if v.deployRequest.WorkloadType != nil {
				workloadType = *v.deployRequest.WorkloadType
			}

			machine := controlapi.MachineSummary{
				Id:      v.vmmID,
				Healthy: true, // TODO cache last health status
				Uptime:  myUptime(now.Sub(v.machineStarted)),
				Workload: controlapi.WorkloadSummary{
					Name:         v.deployRequest.DecodedClaims.Subject,
					Description:  desc,
					Runtime:      myUptime(now.Sub(v.workloadStarted)),
					WorkloadType: workloadType,
					//Hash:         v.deployedWorkload.DecodedClaims.Data["hash"].(string),
				},
			}

			machines = append(machines, machine)
		}
	}
	return machines
}

func validateIssuer(issuer string, validIssuers []string) bool {
	if len(validIssuers) == 0 {
		return true
	}
	return slices.Contains(validIssuers, issuer)
}

// This is the same uptime code as the NATS server, for consistency
func myUptime(d time.Duration) string {
	// Just use total seconds for uptime, and display days / years
	tsecs := d / time.Second
	tmins := tsecs / 60
	thrs := tmins / 60
	tdays := thrs / 24
	tyrs := tdays / 365

	if tyrs > 0 {
		return fmt.Sprintf("%dy%dd%dh%dm%ds", tyrs, tdays%365, thrs%24, tmins%60, tsecs%60)
	}
	if tdays > 0 {
		return fmt.Sprintf("%dd%dh%dm%ds", tdays, thrs%24, tmins%60, tsecs%60)
	}
	if thrs > 0 {
		return fmt.Sprintf("%dh%dm%ds", thrs, tmins%60, tsecs%60)
	}
	if tmins > 0 {
		return fmt.Sprintf("%dm%ds", tmins, tsecs%60)
	}
	return fmt.Sprintf("%ds", tsecs)
}

func respondFail(responseType string, m *nats.Msg, reason string) {
	env := controlapi.NewEnvelope(responseType, []byte{}, &reason)
	jenv, _ := json.Marshal(env)
	_ = m.Respond(jenv)
}

func extractNamespace(subject string) (string, error) {
	tokens := strings.Split(subject, ".")
	// we need at least $NEX.{op}.{namespace}
	if len(tokens) < 3 {
		// this shouldn't ever happen if our subscriptions are defined properly
		return "", errors.Errorf("Invalid subject - could not detect a namespace")
	}
	return tokens[2], nil
}
