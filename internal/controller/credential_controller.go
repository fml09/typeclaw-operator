package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/credential"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

const (
	credentialRequestFinalizer = "typeclaw.fml09.io/credential-request"
	credentialApprovalSuffix   = "-approval"
	credentialRequeue          = 5 * time.Second
)

// SecretMetadata is the only Secret representation accepted by this
// controller. A metadata client is used so Secret.Data never enters operator
// memory or crosses the broker/controller boundary.
type SecretMetadata struct {
	UID             string
	ResourceVersion string
}

// SecretMetadataReader reads only Kubernetes object metadata for a Secret.
type SecretMetadataReader interface {
	GetSecretMetadata(ctx context.Context, namespace, name string) (SecretMetadata, error)
}

// CredentialRequestReconciler consumes one broker ticket into exactly one
// Restricted Credential Runner Job. It never retries a consumed ticket under a
// new Pod identity and never projects credentials into the Managed Runtime.
type CredentialRequestReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	SecretMetadata SecretMetadataReader
	Now            func() time.Time
}

func (r *CredentialRequestReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *CredentialRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	request := &v1alpha1.CredentialRequest{}
	if err := r.Get(ctx, req.NamespacedName, request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if request.DeletionTimestamp != nil {
		return ctrl.Result{}, r.revokeAndRemoveFinalizer(ctx, request)
	}
	if credential.IsTerminal(request.Status.Phase) {
		if err := r.cleanupRunnerNetworkPolicy(ctx, request); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.removeFinalizer(ctx, request)
	}
	if !hasFinalizer(request, credentialRequestFinalizer) {
		request.Finalizers = append(request.Finalizers, credentialRequestFinalizer)
		if err := r.Update(ctx, request); err != nil {
			return ctrl.Result{}, err
		}
	}

	instance := &v1alpha1.TypeClawInstance{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Spec.Instance}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
				status.Phase = v1alpha1.CredentialPhaseDenied
				status.ErrorCode = credential.ErrorCodePolicyDenied
			})
		}
		return ctrl.Result{}, err
	}
	if request.Name != credential.RequestName(request.Spec.TicketDigest) {
		return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseDenied
			status.ErrorCode = credential.ErrorCodePolicyDenied
		})
	}
	if err := r.bindRequest(ctx, request, instance); err != nil {
		return ctrl.Result{}, err
	}
	if credential.IsTerminal(request.Status.Phase) {
		return ctrl.Result{}, nil
	}

	specDigest, err := credentialSpecDigest(request.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	if request.Status.SpecDigest != "" && request.Status.SpecDigest != specDigest {
		return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseDenied
			status.ErrorCode = credential.ErrorCodePolicyDenied
		})
	}
	if request.Spec.ExpiresAt.Time.IsZero() || !r.now().Before(request.Spec.ExpiresAt.Time) {
		return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseDenied
			status.SpecDigest = specDigest
			status.ErrorCode = credential.ErrorCodeTicketExpired
		})
	}

	mode, err := credential.AuthorizePolicy(
		instance.Spec.CredentialPolicy,
		request.Spec.Consumer,
		request.Spec.Operation,
		request.Spec.Repository,
	)
	if err != nil {
		if request.Status.RunnerName != "" {
			return ctrl.Result{}, r.unknownOutcome(ctx, request, err)
		}
		return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseDenied
			status.SpecDigest = specDigest
			status.ErrorCode = credential.ErrorCodePolicyDenied
		})
	}
	if !sameSecretBinding(instance.Spec.CredentialPolicy.Secret, request.Spec.SecretBinding) {
		if request.Status.RunnerName != "" {
			return ctrl.Result{}, r.unknownOutcome(ctx, request, credential.ErrSecretBinding)
		}
		return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseDenied
			status.SpecDigest = specDigest
			status.ErrorCode = credential.ErrorCodeSecretBinding
		})
	}
	if mode == v1alpha1.CredentialAccessConfirm {
		approval, approvalErr := r.approval(ctx, request)
		if approvalErr != nil {
			if apierrors.IsNotFound(approvalErr) {
				if err := r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
					status.Phase = v1alpha1.CredentialPhasePendingApproval
					status.SpecDigest = specDigest
				}); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: credentialRequeue}, nil
			}
			return ctrl.Result{}, approvalErr
		}
		if approval.Spec.Decision == v1alpha1.CredentialDecisionDeny {
			return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
				status.Phase = v1alpha1.CredentialPhaseDenied
				status.SpecDigest = specDigest
				status.ErrorCode = credential.ErrorCodeApprovalDenied
			})
		}
		if approval.Spec.Decision != v1alpha1.CredentialDecisionApprove {
			return ctrl.Result{RequeueAfter: credentialRequeue}, nil
		}
	}
	secretRef := request.Spec.SecretBinding
	metadata, metadataErr := r.secretMetadata(ctx, request, secretRef)
	if metadataErr != nil {
		if errors.Is(metadataErr, credential.ErrSecretBinding) {
			return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
				status.Phase = v1alpha1.CredentialPhaseDenied
				status.SpecDigest = specDigest
				status.ErrorCode = credential.ErrorCodeSecretBinding
			})
		}
		if request.Status.RunnerName != "" {
			return ctrl.Result{}, r.unknownOutcome(ctx, request, metadataErr)
		}
		if err := r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhasePending
			status.SpecDigest = specDigest
			status.ErrorCode = credential.ErrorCodeSecretBinding
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: credentialRequeue}, nil
	}

	if request.Status.RunnerName != "" && (metadata.UID != request.Status.SecretUID || metadata.ResourceVersion != request.Status.SecretResourceVersion) {
		return ctrl.Result{}, r.unknownOutcome(ctx, request, errors.New("credential Secret binding changed"))
	}
	if err := r.ensureRunnerNetworkPolicy(ctx, instance, request); err != nil {
		return ctrl.Result{}, err
	}
	if request.Status.RunnerName != "" {
		return r.observeRunner(ctx, request)
	}

	jobName := resources.CredentialRunnerJobName(request.Name)
	var existingJob batchv1.Job
	jobErr := r.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: jobName}, &existingJob)
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return ctrl.Result{}, jobErr
	}

	if err := r.consumeTicket(ctx, request, specDigest, metadata, jobName); err != nil {
		return ctrl.Result{}, err
	}
	if apierrors.IsNotFound(jobErr) {
		job, err := resources.CredentialRunnerJob(instance, request)
		if err != nil {
			return ctrl.Result{}, r.unknownOutcome(ctx, request, err)
		}
		if err := r.own(request, job); err != nil {

			return ctrl.Result{}, r.unknownOutcome(ctx, request, err)
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Error(err, "Credential Runner creation failed", "request", request.Name)
			return ctrl.Result{}, r.unknownOutcome(ctx, request, errors.New("Runner creation failed"))
		}
	}
	return ctrl.Result{RequeueAfter: credentialRequeue}, nil
}
func hasFinalizer(request *v1alpha1.CredentialRequest, want string) bool {
	for _, finalizer := range request.Finalizers {
		if finalizer == want {
			return true
		}
	}
	return false
}

func (r *CredentialRequestReconciler) revokeAndRemoveFinalizer(ctx context.Context, request *v1alpha1.CredentialRequest) error {
	if request.Status.RunnerName != "" && !credential.IsTerminal(request.Status.Phase) {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: request.Status.RunnerName, Namespace: request.Namespace,
		}}
		if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseUnknownOutcome
			status.ErrorCode = credential.ErrorCodeRunnerFailed
		}); err != nil {
			return err
		}
	}
	if err := r.cleanupRunnerNetworkPolicy(ctx, request); err != nil {
		return err
	}
	return r.removeFinalizer(ctx, request)
}

func (r *CredentialRequestReconciler) removeFinalizer(ctx context.Context, request *v1alpha1.CredentialRequest) error {
	if !hasFinalizer(request, credentialRequestFinalizer) {
		return nil
	}
	finalizers := request.Finalizers[:0]
	for _, finalizer := range request.Finalizers {
		if finalizer != credentialRequestFinalizer {
			finalizers = append(finalizers, finalizer)
		}
	}
	request.Finalizers = finalizers
	return r.Update(ctx, request)
}
func (r *CredentialRequestReconciler) ensureRunnerNetworkPolicy(ctx context.Context, instance *v1alpha1.TypeClawInstance, request *v1alpha1.CredentialRequest) error {
	policy := resources.CredentialRunnerNetworkPolicy(instance, request)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		desired := resources.CredentialRunnerNetworkPolicy(instance, request)
		policy.Spec = desired.Spec
		policy.Labels = desired.Labels
		policy.Annotations = desired.Annotations
		return r.own(request, policy)
	}); err != nil {
		return fmt.Errorf("apply Credential Runner network policy: %w", err)
	}
	return nil
}
func (r *CredentialRequestReconciler) cleanupRunnerNetworkPolicy(ctx context.Context, request *v1alpha1.CredentialRequest) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: resources.CredentialRunnerNetworkPolicyName(request.Name), Namespace: request.Namespace,
	}}
	if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *CredentialRequestReconciler) bindRequest(ctx context.Context, request *v1alpha1.CredentialRequest, instance *v1alpha1.TypeClawInstance) error {
	owner := metav1.GetControllerOf(request)
	if owner != nil && (owner.UID != instance.UID || owner.Name != instance.Name) {
		return r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseDenied
			status.ErrorCode = credential.ErrorCodePolicyDenied
		})
	}
	if owner == nil {
		if err := controllerutil.SetControllerReference(instance, request, r.Scheme); err != nil {
			return err
		}
		if err := r.Update(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func sameSecretBinding(left, right v1alpha1.CredentialSecretRef) bool {
	return left == right
}
func (r *CredentialRequestReconciler) approval(ctx context.Context, request *v1alpha1.CredentialRequest) (*v1alpha1.CredentialApproval, error) {

	approval := &v1alpha1.CredentialApproval{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: request.Namespace,
		Name:      request.Name + credentialApprovalSuffix,
	}, approval)
	if err != nil {
		return nil, err
	}
	if approval.Spec.RequestName != request.Name {
		return nil, errors.New("approval targets a different request")
	}
	return approval, nil
}

func (r *CredentialRequestReconciler) secretMetadata(ctx context.Context, request *v1alpha1.CredentialRequest, ref v1alpha1.CredentialSecretRef) (SecretMetadata, error) {
	if r.SecretMetadata == nil {
		return SecretMetadata{}, errors.New("Secret metadata reader is unavailable")
	}
	metadata, err := r.SecretMetadata.GetSecretMetadata(ctx, request.Namespace, ref.Name)
	if err != nil {
		return SecretMetadata{}, err
	}
	if metadata.UID != ref.UID || metadata.ResourceVersion != ref.ResourceVersion {
		return SecretMetadata{}, fmt.Errorf("%w: metadata does not match policy binding", credential.ErrSecretBinding)
	}
	return metadata, nil
}

func (r *CredentialRequestReconciler) consumeTicket(ctx context.Context, request *v1alpha1.CredentialRequest, specDigest string, metadata SecretMetadata, jobName string) error {
	base := request.DeepCopy()
	request.Status.Phase = v1alpha1.CredentialPhaseTicketConsumed
	request.Status.SpecDigest = specDigest
	request.Status.RunnerName = jobName
	request.Status.SecretUID = metadata.UID
	request.Status.SecretResourceVersion = metadata.ResourceVersion
	request.Status.ErrorCode = ""
	request.Status.TicketConsumedAt = &metav1.Time{Time: r.now()}
	if err := r.Status().Patch(ctx, request, client.MergeFrom(base)); err != nil {
		if apierrors.IsConflict(err) {
			return err
		}
		return fmt.Errorf("consume credential ticket: %w", err)
	}
	return nil
}

func (r *CredentialRequestReconciler) observeRunner(ctx context.Context, request *v1alpha1.CredentialRequest) (ctrl.Result, error) {
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Status.RunnerName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.unknownOutcome(ctx, request, errors.New("Runner Job disappeared after ticket consumption"))
		}
		return ctrl.Result{}, err
	}
	if job.Status.Succeeded > 0 {
		result, podUID, resultErr := r.runnerResult(ctx, request)
		if resultErr != nil {
			return ctrl.Result{}, r.unknownOutcome(ctx, request, resultErr)
		}
		if result.ErrorCode != "" {
			return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
				status.Phase = v1alpha1.CredentialPhaseFailed
				status.PodUID = podUID
				status.ErrorCode = result.ErrorCode
			})
		}
		return ctrl.Result{}, r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseSucceeded
			status.PodUID = podUID
			status.Result = result.Result
			status.ErrorCode = ""
		})
	}
	if job.Status.Failed > 0 {
		return ctrl.Result{}, r.unknownOutcome(ctx, request, errors.New("Runner failed; outcome may be unknown"))
	}
	if request.Status.Phase != v1alpha1.CredentialPhaseRunning {
		podUID, err := r.runnerPodUID(ctx, request)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
			status.Phase = v1alpha1.CredentialPhaseRunning
			if podUID != "" {
				status.PodUID = podUID
			}
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: credentialRequeue}, nil
}
func (r *CredentialRequestReconciler) runnerResult(ctx context.Context, request *v1alpha1.CredentialRequest) (credential.RunnerResult, string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(request.Namespace),
		client.MatchingLabels{"typeclaw.fml09.io/credential-request": request.Name},
	); err != nil {
		return credential.RunnerResult{}, "", err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != resources.CredentialRunnerContainerName || status.State.Terminated == nil {
				continue
			}
			if status.State.Terminated.Message == "" {
				return credential.RunnerResult{}, string(pod.UID), errors.New("Runner result channel is empty")
			}
			result, err := credential.DecodeRunnerResult(status.State.Terminated.Message)
			return result, string(pod.UID), err
		}
	}
	return credential.RunnerResult{}, "", errors.New("Runner result Pod is unavailable")
}

func (r *CredentialRequestReconciler) runnerPodUID(ctx context.Context, request *v1alpha1.CredentialRequest) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(request.Namespace),
		client.MatchingLabels{"typeclaw.fml09.io/credential-request": request.Name},
	); err != nil {
		return "", err
	}
	for i := range pods.Items {
		if pods.Items[i].UID != "" {
			return string(pods.Items[i].UID), nil
		}
	}
	return "", nil
}

func (r *CredentialRequestReconciler) unknownOutcome(ctx context.Context, request *v1alpha1.CredentialRequest, cause error) error {
	if request.Status.RunnerName != "" {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: request.Status.RunnerName, Namespace: request.Namespace}}
		if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	errorCode := credential.ErrorCodeRunnerFailed
	if errors.Is(cause, credential.ErrSecretBinding) {
		errorCode = credential.ErrorCodeSecretBinding
	}
	return r.setStatus(ctx, request, func(status *v1alpha1.CredentialRequestStatus) {
		status.Phase = v1alpha1.CredentialPhaseUnknownOutcome
		status.ErrorCode = errorCode
	})
}

func (r *CredentialRequestReconciler) setStatus(ctx context.Context, request *v1alpha1.CredentialRequest, mutate func(*v1alpha1.CredentialRequestStatus)) error {
	base := request.DeepCopy()
	mutate(&request.Status)
	request.Status.ObservedGeneration = request.Generation
	if err := r.Status().Patch(ctx, request, client.MergeFrom(base)); err != nil {
		return err
	}
	return nil
}

func (r *CredentialRequestReconciler) own(owner *v1alpha1.CredentialRequest, object client.Object) error {
	if err := controllerutil.SetControllerReference(owner, object, r.Scheme); err != nil {
		return err
	}
	return nil
}

func credentialSpecDigest(spec v1alpha1.CredentialRequestSpec) (string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// SetupWithManager wires Job ownership so completion and failure become
// observable without polling a generic API or replaying a ticket.
func (r *CredentialRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.CredentialRequest{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
