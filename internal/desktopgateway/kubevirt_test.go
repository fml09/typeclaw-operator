package desktopgateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

func TestDecodeVirtualMachineReadsOnlyTheFieldsTheGatewayActsOn(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":              testName,
			"namespace":         testNamespace,
			"uid":               "vm-uid",
			"deletionTimestamp": "2026-09-02T10:00:00Z",
		},
		"spec": map[string]any{"runStrategy": RunStrategyManual},
		"status": map[string]any{
			"printableStatus": VirtualMachineStatusRunning,
			"stateChangeRequests": []any{
				map[string]any{"action": "Stop"},
			},
		},
	}}
	vm, err := decodeVirtualMachine(object)
	if err != nil {
		t.Fatal(err)
	}
	if vm.UID != "vm-uid" || !vm.Deleting || vm.RunStrategy != RunStrategyManual ||
		vm.PrintableStatus != VirtualMachineStatusRunning {
		t.Fatalf("decoded VM = %#v", vm)
	}
	if len(vm.StateChangeRequests) != 1 || vm.StateChangeRequests[0].Action != "Stop" {
		t.Fatalf("decoded state change requests = %#v", vm.StateChangeRequests)
	}
}

func TestDecodeVirtualMachineTreatsAbsentStatusAsUnknown(t *testing.T) {
	vm, err := decodeVirtualMachine(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": testName},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if vm.PrintableStatus != "" || vm.Deleting || len(vm.StateChangeRequests) != 0 {
		t.Fatalf("decoded VM = %#v", vm)
	}
	// An unset status is never "Stopped": nothing may read it as a settled
	// state that would make a stop conflict look idempotent.
	if stableStoppedAfterStopConflict(vm, nil) {
		t.Fatal("a VM without status was treated as settled")
	}
}

func TestDecodeVirtualMachineRejectsMistypedFields(t *testing.T) {
	_, err := decodeVirtualMachine(&unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"printableStatus": 7},
	}})
	if err == nil {
		t.Fatal("a numeric printableStatus was accepted")
	}
	_, err = decodeVirtualMachine(&unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"stateChangeRequests": []any{"Stop"}},
	}})
	if err == nil {
		t.Fatal("a malformed stateChangeRequests entry was accepted")
	}
	if _, err := decodeVirtualMachine(nil); err == nil {
		t.Fatal("a missing object was accepted")
	}
}

func TestDecodeVirtualMachineInstance(t *testing.T) {
	vmi, err := decodeVirtualMachineInstance(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"uid": "vmi-uid"},
		"status":   map[string]any{"phase": VirtualMachineInstanceRunning},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if vmi.UID != "vmi-uid" || vmi.Phase != VirtualMachineInstanceRunning || vmi.Deleting {
		t.Fatalf("decoded VMI = %#v", vmi)
	}
	if _, err := decodeVirtualMachineInstance(nil); err == nil {
		t.Fatal("a missing object was accepted")
	}
}

// apiServerStub answers the KubeVirt paths the gateway uses, recording what it
// was asked for so the request shape itself is under test.
type apiServerStub struct {
	requests []string
	bodies   []string
	handler  func(w http.ResponseWriter, r *http.Request)
}

func (s *apiServerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.requests = append(s.requests, r.Method+" "+r.URL.Path+queryOf(r))
	s.bodies = append(s.bodies, string(body))
	s.handler(w, r)
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func writeStatusError(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"`+
		message+`","reason":"`+string(reason)+`","code":`+strconv.Itoa(code)+`}`)
}

func newStubbedClient(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (KubeVirtClient, *apiServerStub) {
	t.Helper()
	stub := &apiServerStub{handler: handler}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	client, err := NewKubeVirtClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client, stub
}

func TestStartAndStopUseTheKubeVirtSubresourceVerbs(t *testing.T) {
	client, stub := newStubbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	if err := client.Start(context.Background(), testNamespace, testName); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(context.Background(), testNamespace, testName); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /apis/subresources.kubevirt.io/v1/namespaces/desktops/virtualmachines/inst-desktop/start",
		"PUT /apis/subresources.kubevirt.io/v1/namespaces/desktops/virtualmachines/inst-desktop/stop",
	}
	for index, request := range want {
		if stub.requests[index] != request {
			t.Fatalf("request %d = %q, want %q", index, stub.requests[index], request)
		}
		if stub.bodies[index] != "" {
			t.Fatalf("request %d carried body %q, want an empty body", index, stub.bodies[index])
		}
	}
}

func TestPowerVerbsPreserveTheAPIServerReason(t *testing.T) {
	client, _ := newStubbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStatusError(w, http.StatusForbidden, metav1.StatusReasonForbidden,
			"virtualmachines.subresources.kubevirt.io is forbidden")
	})
	err := client.Start(context.Background(), testNamespace, testName)
	if err == nil {
		t.Fatal("a forbidden start was reported as success")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("start error = %v, want a Forbidden API error", err)
	}
	// Ticket #20 depends on this classification surviving the REST client.
	if !definitivePowerRejection(err) {
		t.Fatal("a Forbidden start was not classified as a definite rejection")
	}
}

func TestScreenshotReadsThePNGSubresource(t *testing.T) {
	frame := encodePNG(t, 8, 8)
	client, stub := newStubbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(frame)
	})
	raw, err := client.Screenshot(context.Background(), testNamespace, testName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, frame) {
		t.Fatalf("screenshot returned %d bytes, want the %d byte frame", len(raw), len(frame))
	}
	want := "GET /apis/subresources.kubevirt.io/v1/namespaces/desktops/virtualmachineinstances/inst-desktop/vnc/screenshot?moveCursor=false"
	if stub.requests[0] != want {
		t.Fatalf("screenshot request = %q, want %q", stub.requests[0], want)
	}
}

func TestScreenshotReportsAMissingVMI(t *testing.T) {
	client, _ := newStubbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStatusError(w, http.StatusNotFound, metav1.StatusReasonNotFound, "virtualmachineinstances not found")
	})
	_, err := client.Screenshot(context.Background(), testNamespace, testName)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("screenshot error = %v, want a NotFound API error", err)
	}
	if got := screenshotReadStatus(err); got != http.StatusConflict {
		t.Fatalf("screenshotReadStatus() = %d, want 409", got)
	}
}

func TestVMReadsGoThroughTheDynamicClient(t *testing.T) {
	client, stub := newStubbedClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/kubevirt.io/v1/namespaces/desktops/virtualmachineinstances/inst-desktop" {
			writeStatusError(w, http.StatusNotFound, metav1.StatusReasonNotFound, "virtualmachineinstances not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine",`+
			`"metadata":{"name":"inst-desktop","namespace":"desktops","uid":"vm-uid"},`+
			`"spec":{"runStrategy":"Manual"},"status":{"printableStatus":"Stopped"}}`)
	})

	vm, err := client.GetVM(context.Background(), testNamespace, testName)
	if err != nil {
		t.Fatal(err)
	}
	if vm.UID != "vm-uid" || vm.RunStrategy != RunStrategyManual || vm.PrintableStatus != VirtualMachineStatusStopped {
		t.Fatalf("decoded VM = %#v", vm)
	}
	if stub.requests[0] != "GET /apis/kubevirt.io/v1/namespaces/desktops/virtualmachines/inst-desktop" {
		t.Fatalf("VM read request = %q", stub.requests[0])
	}

	if _, err := client.GetVMI(context.Background(), testNamespace, testName); !apierrors.IsNotFound(err) {
		t.Fatalf("VMI read error = %v, want a NotFound API error", err)
	}
}

func TestNewKubeVirtClientRequiresAConfig(t *testing.T) {
	if _, err := NewKubeVirtClient(nil); err == nil {
		t.Fatal("a nil REST config was accepted")
	}
}
