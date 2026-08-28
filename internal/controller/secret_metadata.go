package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadataclient "k8s.io/client-go/metadata"
)

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// KubernetesSecretMetadataReader uses the metadata API, not the typed Secret
// API. The returned object cannot contain Secret.Data by construction.
type KubernetesSecretMetadataReader struct {
	Client metadataclient.Interface
}

func (r KubernetesSecretMetadataReader) GetSecretMetadata(ctx context.Context, namespace, name string) (SecretMetadata, error) {
	if r.Client == nil {
		return SecretMetadata{}, fmt.Errorf("metadata client unavailable")
	}
	object, err := r.Client.Resource(secretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return SecretMetadata{}, err
	}
	return SecretMetadata{
		UID:             string(object.UID),
		ResourceVersion: object.ResourceVersion,
	}, nil
}
