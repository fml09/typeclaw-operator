package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/credential"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

type fakeSecretMetadata struct {
	metadata SecretMetadata
	err      error
}

func (f fakeSecretMetadata) GetSecretMetadata(context.Context, string, string) (SecretMetadata, error) {
	return f.metadata, f.err
}

func credentialControllerPolicy(mode v1alpha1.CredentialAccessMode) *v1alpha1.CredentialPolicySpec {
	return &v1alpha1.CredentialPolicySpec{
		Secret: v1alpha1.CredentialSecretRef{
			Name: "github-credential-1", Key: "token", UID: "secret-uid", ResourceVersion: "17", Immutable: true,
		},
		Consumers: []v1alpha1.CredentialConsumerSpec{{
			Name: "github", Operations: []v1alpha1.CredentialOperation{v1alpha1.CredentialOperationGitHubCreateIssue},
			AccessMode: mode, AllowedRepositories: []string{"fml09/typeclaw"},
		}},
	}
}

func credentialControllerInstance(mode v1alpha1.CredentialAccessMode) *v1alpha1.TypeClawInstance {
	return &v1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "kakao-agent", Namespace: "agents", UID: types.UID("instance-uid")},
		Spec:       v1alpha1.TypeClawInstanceSpec{CredentialPolicy: credentialControllerPolicy(mode)},
	}
}

func credentialControllerRequest() *v1alpha1.CredentialRequest {
	return &v1alpha1.CredentialRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "credential-1234567890abcdef12345678", Namespace: "agents"},
		Spec: v1alpha1.CredentialRequestSpec{
			Instance: "kakao-agent", Consumer: "github", Operation: v1alpha1.CredentialOperationGitHubCreateIssue,
			TicketDigest: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			ExpiresAt:    metav1.Time{Time: time.Unix(1700003600, 0)},
			SecretBinding: v1alpha1.CredentialSecretRef{
				Name: "github-credential-1", Key: "token", UID: "secret-uid", ResourceVersion: "17", Immutable: true,
			},
			Repository: "fml09/typeclaw",
			Title:      "hello",
			Body:       "world",
		},
	}
}

func credentialControllerFor(t *testing.T, objects ...client.Object) (*CredentialRequestReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.CredentialRequest{}, &batchv1.Job{}).
		WithObjects(objects...).
		Build()
	return &CredentialRequestReconciler{
		Client:         c,
		Scheme:         c.Scheme(),
		SecretMetadata: fakeSecretMetadata{metadata: SecretMetadata{UID: "secret-uid", ResourceVersion: "17"}},
		Now:            func() time.Time { return time.Unix(1700000000, 0) },
	}, c
}

func reconcileCredential(t *testing.T, r *CredentialRequestReconciler, request *v1alpha1.CredentialRequest) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: request.Namespace, Name: request.Name}})
	if err != nil {
		t.Fatalf("CredentialRequest reconcile: %v", err)
	}
	return result
}

func TestCredentialRequestConsumesTicketOnceAndReturnsTypedResult(t *testing.T) {
	instance := credentialControllerInstance(v1alpha1.CredentialAccessPreAuthorized)
	request := credentialControllerRequest()
	r, c := credentialControllerFor(t, instance, request)

	result := reconcileCredential(t, r, request)
	if result.RequeueAfter != credentialRequeue {
		t.Fatalf("initial reconcile requeue = %v, want %v", result.RequeueAfter, credentialRequeue)
	}
	var got v1alpha1.CredentialRequest
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhaseTicketConsumed || got.Status.RunnerName == "" || got.Status.SecretUID != "secret-uid" {
		t.Fatalf("ticket consumption status = %+v", got.Status)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: request.Namespace, Name: got.Status.RunnerName}, &job); err != nil {
		t.Fatalf("Runner Job missing: %v", err)
	}

	// Reconciliation observes the same Job; it never creates a second one.
	reconcileCredential(t, r, &got)
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(request.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("ticket retry/reconcile created %d Runner Jobs", len(jobs.Items))
	}

	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatalf("update Job status: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "credential-runner-pod", Namespace: request.Namespace,
			UID: types.UID("runner-pod-uid"), Labels: map[string]string{"typeclaw.fml09.io/credential-request": request.Name},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: resources.CredentialRunnerContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0,
				Message:  `{"result":{"number":7,"url":"https://github.com/fml09/typeclaw/issues/7","title":"hello"}}`,
			}},
		}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("create result Pod: %v", err)
	}
	reconcileCredential(t, r, &got)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhaseSucceeded || got.Status.PodUID != "runner-pod-uid" || got.Status.Result == nil || got.Status.Result.Number != 7 {
		t.Fatalf("typed result status = %+v", got.Status)
	}
}

func TestCredentialRequestDeniesSecretRotationBeforeRunnerCreation(t *testing.T) {
	instance := credentialControllerInstance(v1alpha1.CredentialAccessPreAuthorized)
	request := credentialControllerRequest()
	r, c := credentialControllerFor(t, instance, request)
	r.SecretMetadata = fakeSecretMetadata{metadata: SecretMetadata{UID: "new-secret-uid", ResourceVersion: "18"}}
	reconcileCredential(t, r, request)

	var got v1alpha1.CredentialRequest
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhaseDenied || got.Status.ErrorCode != credential.ErrorCodeSecretBinding {
		t.Fatalf("rotated Secret must deny request: %+v", got.Status)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(request.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("rotated Secret must not create Runner Job: %d", len(jobs.Items))
	}
}
func TestCredentialRequestInvalidatesConsumedTicketWhenPolicyRotates(t *testing.T) {
	instance := credentialControllerInstance(v1alpha1.CredentialAccessPreAuthorized)
	request := credentialControllerRequest()
	r, c := credentialControllerFor(t, instance, request)
	reconcileCredential(t, r, request)

	var liveInstance v1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(instance), &liveInstance); err != nil {
		t.Fatal(err)
	}
	liveInstance.Spec.CredentialPolicy.Secret.UID = "new-secret-uid"
	liveInstance.Spec.CredentialPolicy.Secret.ResourceVersion = "18"
	if err := c.Update(context.Background(), &liveInstance); err != nil {
		t.Fatal(err)
	}
	r.SecretMetadata = fakeSecretMetadata{metadata: SecretMetadata{UID: "new-secret-uid", ResourceVersion: "18"}}

	var got v1alpha1.CredentialRequest
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	reconcileCredential(t, r, &got)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhaseUnknownOutcome || got.Status.ErrorCode != credential.ErrorCodeSecretBinding {
		t.Fatalf("rotated consumed binding = %+v", got.Status)
	}
	var job batchv1.Job
	err := c.Get(context.Background(), types.NamespacedName{Namespace: request.Namespace, Name: resources.CredentialRunnerJobName(request.Name)}, &job)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("consumed Runner Job must be revoked, err=%v job=%+v", err, job)
	}
}

func TestCredentialRequestConfirmRequiresSeparateApproval(t *testing.T) {
	instance := credentialControllerInstance(v1alpha1.CredentialAccessConfirm)
	request := credentialControllerRequest()
	r, c := credentialControllerFor(t, instance, request)
	result := reconcileCredential(t, r, request)
	if result.RequeueAfter != credentialRequeue {
		t.Fatalf("pending approval requeue = %v", result.RequeueAfter)
	}
	var got v1alpha1.CredentialRequest
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhasePendingApproval {
		t.Fatalf("phase = %q, want pending approval", got.Status.Phase)
	}
	approval := &v1alpha1.CredentialApproval{
		ObjectMeta: metav1.ObjectMeta{Name: request.Name + credentialApprovalSuffix, Namespace: request.Namespace},
		Spec:       v1alpha1.CredentialApprovalSpec{RequestName: request.Name, Decision: v1alpha1.CredentialDecisionApprove},
	}
	if err := c.Create(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	reconcileCredential(t, r, &got)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhaseTicketConsumed {
		t.Fatalf("approved request phase = %q, want ticket consumed", got.Status.Phase)
	}
}

func TestCredentialRequestMissingRunnerBecomesUnknownOutcome(t *testing.T) {
	instance := credentialControllerInstance(v1alpha1.CredentialAccessPreAuthorized)
	request := credentialControllerRequest()
	request.Status = v1alpha1.CredentialRequestStatus{
		Phase: v1alpha1.CredentialPhaseTicketConsumed, RunnerName: resources.CredentialRunnerJobName(request.Name),
		SecretUID: "secret-uid", SecretResourceVersion: "17",
	}
	r, c := credentialControllerFor(t, instance, request)
	reconcileCredential(t, r, request)
	var got v1alpha1.CredentialRequest
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(request), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.CredentialPhaseUnknownOutcome {
		t.Fatalf("missing Runner must become UnknownOutcome, got %q", got.Status.Phase)
	}
}
