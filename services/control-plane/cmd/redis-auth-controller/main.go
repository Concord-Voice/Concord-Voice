// Package main serves the local managed Redis authorization controller.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
)

const maxControlLine = 16 * 1024
const maxControlReplyLine = 64 * 1024
const controllerUID = 10004

type controlRequest struct {
	Operation    string              `json:"operation"`
	RotationID   string              `json:"rotationId,omitempty"`
	Service      string              `json:"service,omitempty"`
	Slot         string              `json:"slot,omitempty"`
	ContainerID  string              `json:"containerId,omitempty"`
	InstanceID   string              `json:"instanceId,omitempty"`
	ReplacedID   string              `json:"replacedContainerId,omitempty"`
	Generation   uint64              `json:"generation,omitempty"`
	Instances    map[string][]string `json:"instances,omitempty"`
	Targets      map[string]uint64   `json:"targets,omitempty"`
	OldUsernames map[string]string   `json:"oldUsernames,omitempty"`
	Args         []string            `json:"args,omitempty"`
}

func decodeControlLine(raw []byte, dst any) error {
	return managedredis.DecodeBoundedJSON(raw, dst)
}

func handleAck(ctx context.Context, controller *managedredis.Controller, slot string, peerUID uint32, raw []byte) []byte {
	var ack managedredis.AppliedAck
	if controller == nil || decodeControlLine(raw, &ack) != nil ||
		controller.RecordAck(ctx, slot, peerUID, ack) != nil {
		return []byte("{\"error\":\"ack-rejected\"}\n")
	}
	return []byte("{\"ok\":true}\n")
}

type controlField uint16

const (
	controlRotation controlField = 1 << iota
	controlService
	controlSlot
	controlContainer
	controlInstance
	controlReplaced
	controlGeneration
	controlInstances
	controlTargets
	controlOldUsernames
	controlArgs
)

func hasUnexpectedControlFields(request controlRequest, allowed controlField) bool {
	fields := [...]struct {
		field   controlField
		present bool
	}{
		{controlRotation, request.RotationID != ""}, {controlService, request.Service != ""},
		{controlSlot, request.Slot != ""}, {controlContainer, request.ContainerID != ""},
		{controlInstance, request.InstanceID != ""}, {controlReplaced, request.ReplacedID != ""},
		{controlGeneration, request.Generation != 0}, {controlInstances, len(request.Instances) != 0},
		{controlTargets, len(request.Targets) != 0}, {controlOldUsernames, len(request.OldUsernames) != 0},
		{controlArgs, len(request.Args) != 0},
	}
	for _, item := range fields {
		if item.present && allowed&item.field == 0 {
			return true
		}
	}
	return false
}

func handleControl(ctx context.Context, controller *managedredis.Controller, peerUID uint32, raw []byte) []byte {
	if controller == nil {
		return controlRejected()
	}
	var request controlRequest
	if decodeControlLine(raw, &request) != nil {
		return controlRejected()
	}
	if peerUID != 0 && (peerUID != controllerUID || request.Operation != "status") {
		return controlRejected()
	}
	return handleControlOperation(ctx, controller, request)
}

func handleRecoveryControl(ctx context.Context, controller *managedredis.Controller, peerUID uint32, raw []byte) []byte {
	var request controlRequest
	if controller == nil || decodeControlLine(raw, &request) != nil || hasUnexpectedControlFields(request, 0) {
		return controlRejected()
	}
	if request.Operation == "status" && (peerUID == 0 || peerUID == controllerUID) {
		state := "recovery-complete"
		if controller.RevocationPending() || controller.ReconciliationPending() {
			state = "resume-required"
		}
		return marshalControlStatus(controller, state)
	}
	if request.Operation == "resume" && peerUID == 0 && controller.RevocationPending() && controller.ResumeFinalize(ctx) == nil {
		return marshalControlStatus(controller, "recovery-complete")
	}
	return controlRejected()
}

func handleControlOperation(ctx context.Context, controller *managedredis.Controller, request controlRequest) []byte {
	switch request.Operation {
	case "status":
		return controlStatus(controller, request)
	case "probe":
		return controlProbe(ctx, controller, request)
	case "register":
		return controlRegister(ctx, controller, request)
	case "bind":
		return controlBind(ctx, controller, request)
	case "replace-ready":
		return controlReplaceReady(controller, request)
	case "finalize":
		return controlFinalize(ctx, controller, request)
	case "resume":
		return controlResume(ctx, controller, request)
	case "redis":
		if hasUnexpectedControlFields(request, controlArgs) {
			return controlRejected()
		}
		return handleRedisControl(ctx, controller, request.Args)
	default:
		return controlRejected()
	}
}

func controlRejected() []byte { return []byte("{\"error\":\"control-rejected\"}\n") }

func controlStatus(controller *managedredis.Controller, request controlRequest) []byte {
	if hasUnexpectedControlFields(request, 0) {
		return controlRejected()
	}
	return marshalControlStatus(controller, "")
}

func controlProbe(ctx context.Context, controller *managedredis.Controller, request controlRequest) []byte {
	state, _, _, _ := controller.StatusSnapshot()
	if state == "failed" || hasUnexpectedControlFields(request, 0) || controller.ProbeConfigured(ctx) != nil {
		return controlRejected()
	}
	return marshalControlStatus(controller, "proved")
}

func controlRegister(ctx context.Context, controller *managedredis.Controller, request controlRequest) []byte {
	if hasUnexpectedControlFields(request, controlRotation|controlInstances|controlTargets) || controller.RegisterExpected(ctx, request.RotationID, request.Instances, request.Targets) != nil {
		return controlRejected()
	}
	return marshalControlStatus(controller, "")
}

func controlBind(ctx context.Context, controller *managedredis.Controller, request controlRequest) []byte {
	if hasUnexpectedControlFields(request, controlSlot|controlContainer|controlInstance|controlReplaced) || controller.BindInstance(ctx, request.Slot, request.ContainerID, request.InstanceID, request.ReplacedID) != nil {
		return controlRejected()
	}
	return marshalControlStatus(controller, "")
}

func controlReplaceReady(controller *managedredis.Controller, request controlRequest) []byte {
	if hasUnexpectedControlFields(request, controlSlot|controlContainer) || controller.ReplaceReady(request.Slot, request.ContainerID) != nil {
		return controlRejected()
	}
	return marshalControlStatus(controller, "")
}

func controlFinalize(ctx context.Context, controller *managedredis.Controller, request controlRequest) []byte {
	if hasUnexpectedControlFields(request, controlRotation|controlOldUsernames) || controller.Finalize(ctx, request.RotationID, request.OldUsernames) != nil {
		return controlRejected()
	}
	return marshalControlStatus(controller, "")
}

func controlResume(ctx context.Context, controller *managedredis.Controller, request controlRequest) []byte {
	if hasUnexpectedControlFields(request, 0) || controller.ResumeFinalize(ctx) != nil {
		return controlRejected()
	}
	return marshalControlStatus(controller, "")
}

func handleRedisControl(ctx context.Context, controller *managedredis.Controller, args []string) []byte {
	output, err := controller.ExecuteOperator(ctx, args)
	if err != nil {
		return controlRejected()
	}
	reply, err := json.Marshal(struct {
		Output string `json:"output"`
	}{Output: output})
	if err != nil || len(reply) > maxControlLine-1 {
		return controlRejected()
	}
	return append(reply, '\n')
}

func marshalControlStatus(controller *managedredis.Controller, stateOverride string) []byte {
	const denied = "{\"error\":\"control-rejected\"}\n"
	state, rotation, targets, instances := controller.StatusSnapshot()
	priorRotation, priorExpected, priorProofs := controller.PriorRegistrationProof()
	if stateOverride != "" {
		state = stateOverride
	}
	reply, err := json.Marshal(struct {
		State           string                                  `json:"state"`
		RotationID      string                                  `json:"rotationId"`
		Targets         map[string]uint64                       `json:"targets"`
		Instances       map[string][]string                     `json:"instances,omitempty"`
		Bindings        map[string]managedredis.InstanceBinding `json:"bindings"`
		PriorRotationID string                                  `json:"priorRotationId,omitempty"`
		PriorExpected   map[string][]string                     `json:"priorExpected,omitempty"`
		PriorAckProofs  []managedredis.PriorAckProof            `json:"priorAckProofs,omitempty"`
	}{State: state, RotationID: rotation, Targets: targets, Instances: instances, Bindings: controller.BindingSnapshot(),
		PriorRotationID: priorRotation, PriorExpected: priorExpected, PriorAckProofs: priorProofs})
	if err != nil || len(reply) > maxControlReplyLine-1 {
		return []byte(denied)
	}
	return append(reply, '\n')
}

func main() {
	validArgs := len(os.Args) == 2 && (os.Args[1] == "serve" || os.Args[1] == "request") || len(os.Args) == 3 && os.Args[1] == "prepare" &&
		(os.Args[2] == "--first-activation" || os.Args[2] == "--recover" || os.Args[2] == "--recover-lost-state")
	if !validArgs {
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "prepare":
		mode := ""
		if len(os.Args) == 3 {
			mode = os.Args[2][2:]
		}
		err = prepare(mode)
	case "request":
		err = request(os.Stdin, os.Stdout, controlSocket)
	default:
		os.Exit(2)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "redis-auth-controller: operation failed")
		os.Exit(1)
	}
}
