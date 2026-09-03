package desktopgateway

import (
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// KubeVirt printable statuses and VMI phases the gateway reasons about. They
// are compared as strings because the gateway reads KubeVirt objects as
// unstructured data and never imports the KubeVirt Go types.
const (
	VirtualMachineStatusRunning   = "Running"
	VirtualMachineStatusStopped   = "Stopped"
	VirtualMachineInstanceRunning = "Running"
	// Succeeded and Failed are the phases a VirtualMachineInstance can never
	// leave. They matter because under runStrategy Manual nothing deletes the
	// object when the guest stops: a desktop that was shut down from inside
	// keeps a final-phase instance until the next start.
	VirtualMachineInstanceSucceeded = "Succeeded"
	VirtualMachineInstanceFailed    = "Failed"
	// RunStrategyManual is the only run strategy the operator renders; the VM
	// is started and stopped exclusively through the subresource API.
	RunStrategyManual = "Manual"
)

// finalInstancePhase reports whether a VirtualMachineInstance has reached a
// terminal phase, which is the same thing to a caller as having no instance at
// all: KubeVirt's stop handler answers the identical Conflict for an absent
// instance and for a final-phase one, so the gateway must read both as
// "nothing is running here".
func finalInstancePhase(phase string) bool {
	return phase == VirtualMachineInstanceSucceeded || phase == VirtualMachineInstanceFailed
}

var (
	// virtualMachineGVR and virtualMachineInstanceGVR address the KubeVirt
	// core API through the dynamic client.
	virtualMachineGVR = schema.GroupVersionResource{
		Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines",
	}
	virtualMachineInstanceGVR = schema.GroupVersionResource{
		Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances",
	}
)

// VirtualMachine is the decoded slice of a KubeVirt VirtualMachine the
// gateway acts on. Everything else in the object is deliberately dropped so
// the rest of the gateway keeps working with typed values.
type VirtualMachine struct {
	UID                 string
	Deleting            bool
	RunStrategy         string
	PrintableStatus     string
	StateChangeRequests []StateChangeRequest
}

// StateChangeRequest is one pending KubeVirt lifecycle request. A non-empty
// list means the VM's observed status has not caught up with an intent, which
// makes any conflict answer ambiguous.
type StateChangeRequest struct {
	Action string
}

// VirtualMachineInstance is the decoded slice of a KubeVirt
// VirtualMachineInstance the gateway acts on.
type VirtualMachineInstance struct {
	UID      string
	Deleting bool
	Phase    string
}

// VNCStream is one open RFB stream to a desktop's VNC subresource. Reads
// deliver the reassembled RFB byte stream; a peer close surfaces as io.EOF.
type VNCStream interface {
	io.ReadWriteCloser
}

// KubeVirtClient is the seam between the Desktop Gateway and KubeVirt. The
// production implementation speaks to the Kubernetes API with client-go only;
// tests substitute an in-memory fake. Every method takes the namespace and
// name explicitly so no implementation carries desktop identity of its own.
type KubeVirtClient interface {
	GetVM(ctx context.Context, namespace, name string) (*VirtualMachine, error)
	GetVMI(ctx context.Context, namespace, name string) (*VirtualMachineInstance, error)
	Start(ctx context.Context, namespace, name string) error
	Stop(ctx context.Context, namespace, name string) error
	Screenshot(ctx context.Context, namespace, name string) ([]byte, error)
	DialVNC(ctx context.Context, namespace, name string) (VNCStream, error)
}

// decodeVirtualMachine projects the unstructured VirtualMachine onto the
// fields the gateway reasons about. A field of the wrong type is reported
// rather than silently read as its zero value, because "Running" and "absent"
// drive opposite power decisions.
func decodeVirtualMachine(object *unstructured.Unstructured) (*VirtualMachine, error) {
	if object == nil {
		return nil, fmt.Errorf("decode VirtualMachine: object is missing")
	}
	vm := &VirtualMachine{
		UID:      string(object.GetUID()),
		Deleting: object.GetDeletionTimestamp() != nil,
	}
	runStrategy, err := nestedString(object.Object, "spec", "runStrategy")
	if err != nil {
		return nil, err
	}
	vm.RunStrategy = runStrategy
	printableStatus, err := nestedString(object.Object, "status", "printableStatus")
	if err != nil {
		return nil, err
	}
	vm.PrintableStatus = printableStatus

	requests, found, err := unstructured.NestedSlice(object.Object, "status", "stateChangeRequests")
	if err != nil {
		return nil, fmt.Errorf("decode VirtualMachine status.stateChangeRequests: %w", err)
	}
	if found {
		for _, entry := range requests {
			mapped, ok := entry.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("decode VirtualMachine status.stateChangeRequests: entry is %T", entry)
			}
			action, err := nestedString(mapped, "action")
			if err != nil {
				return nil, err
			}
			vm.StateChangeRequests = append(vm.StateChangeRequests, StateChangeRequest{Action: action})
		}
	}
	return vm, nil
}

// decodeVirtualMachineInstance projects the unstructured
// VirtualMachineInstance onto the fields the gateway reasons about.
func decodeVirtualMachineInstance(object *unstructured.Unstructured) (*VirtualMachineInstance, error) {
	if object == nil {
		return nil, fmt.Errorf("decode VirtualMachineInstance: object is missing")
	}
	phase, err := nestedString(object.Object, "status", "phase")
	if err != nil {
		return nil, err
	}
	return &VirtualMachineInstance{
		UID:      string(object.GetUID()),
		Deleting: object.GetDeletionTimestamp() != nil,
		Phase:    phase,
	}, nil
}

func nestedString(object map[string]any, fields ...string) (string, error) {
	value, found, err := unstructured.NestedString(object, fields...)
	if err != nil {
		return "", fmt.Errorf("decode %v: %w", fields, err)
	}
	if !found {
		return "", nil
	}
	return value, nil
}
