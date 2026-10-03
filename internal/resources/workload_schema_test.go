package resources

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestGeneratedTmpSizeLimitSchema(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/typeclaw.fml09.io_typeclawinstances.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&internal)
	if err != nil {
		t.Fatalf("generated CRD is not structural: %v", err)
	}
	storage := structural.Properties["spec"].Properties["storage"]
	limit, ok := storage.Properties["tmpSizeLimit"]
	if !ok {
		t.Fatal("generated CRD would prune spec.storage.tmpSizeLimit")
	}
	obj := map[string]interface{}{}
	defaulting.Default(obj, &storage)
	if got := obj["tmpSizeLimit"]; got != "256Mi" {
		t.Fatalf("omitted tmpSizeLimit default = %v, want 256Mi", got)
	}
	validator := cel.NewValidator(&limit, false, 1000000)
	if validator == nil {
		t.Fatal("tmpSizeLimit has no admission validation")
	}
	for _, tc := range []struct {
		name    string
		value   interface{}
		invalid bool
	}{
		{name: "default", value: "256Mi"},
		{name: "four GiB", value: "4Gi"},
		{name: "eight GiB", value: "8Gi"},
		{name: "integer bytes", value: int64(4096)},
		{name: "zero", value: "0", invalid: true},
		{name: "negative", value: "-4Gi", invalid: true},
		{name: "zero integer", value: int64(0), invalid: true},
		{name: "invalid quantity", value: "not-a-size", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs, _ := validator.Validate(context.Background(), field.NewPath("spec", "storage", "tmpSizeLimit"), &limit, tc.value, nil, 1000000)
			if tc.invalid {
				if len(errs) != 1 || !strings.Contains(errs[0].Detail, "tmpSizeLimit must be a positive Kubernetes quantity") {
					t.Fatalf("admission must reject invalid capacity with its recovery message, got %v", errs)
				}
			} else if len(errs) != 0 {
				t.Fatalf("admission rejected supported capacity: %v", errs)
			}
		})
	}
}
