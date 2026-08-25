package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestRelaySidecarEncodesContract(t *testing.T) {
	c := RelaySidecar("agents/kakao-agent", "/run/typeclaw-managed", "ghcr.io/fml09/typeclaw-operator:0.1.0")

	if c.Name != RelayContainerName {
		t.Fatalf("container name = %q, want %q", c.Name, RelayContainerName)
	}
	if len(c.Command) != 1 || c.Command[0] != "/relay" {
		t.Fatalf("command = %v, want [/relay]", c.Command)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if env["TYPECLAW_MANAGED_CONTROL_DIR"].Value != "/run/typeclaw-managed" {
		t.Fatalf("control dir env = %+v", env["TYPECLAW_MANAGED_CONTROL_DIR"])
	}
	if env["TYPECLAW_RUNTIME_ID"].Value != "agents/kakao-agent" {
		t.Fatalf("runtime id env = %+v", env["TYPECLAW_RUNTIME_ID"])
	}
	for name, fieldPath := range map[string]string{
		"POD_NAME":      "metadata.name",
		"POD_NAMESPACE": "metadata.namespace",
	} {
		src := env[name].ValueFrom
		if src == nil || src.FieldRef == nil || src.FieldRef.FieldPath != fieldPath {
			t.Fatalf("%s env = %+v, want fieldRef %s", name, env[name], fieldPath)
		}
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}
	control := mounts["managed-control"]
	if control.MountPath != "/run/typeclaw-managed" || control.ReadOnly {
		t.Fatalf("control dir mount = %+v", control)
	}
	sc := c.SecurityContext
	if sc == nil ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("security context violates Restricted floor: %+v", sc)
	}
}

func TestRelayTokenVolumeProjectsServiceAccountToken(t *testing.T) {
	v := RelayTokenVolume()
	if v.Name != RelayTokenVolumeName {
		t.Fatalf("volume name = %q, want %q", v.Name, RelayTokenVolumeName)
	}
	projected := v.Projected
	if projected == nil || len(projected.Sources) != 1 {
		t.Fatalf("volume must carry exactly one projection: %+v", v)
	}
	token := projected.Sources[0].ServiceAccountToken
	if token == nil || token.Path != RelayTokenFileName {
		t.Fatalf("service account token projection = %+v", token)
	}
	if token.ExpirationSeconds == nil || *token.ExpirationSeconds <= 0 {
		t.Fatalf("expiration seconds = %+v", token.ExpirationSeconds)
	}
}

func TestRelayRBACScopedToOwnPod(t *testing.T) {
	in := instance("kakao-agent", nil)

	sa := RelayServiceAccount(in)
	if sa.Name != "kakao-agent-relay" || sa.Namespace != in.Namespace {
		t.Fatalf("ServiceAccount = %s/%s, want agents/kakao-agent-relay", sa.Namespace, sa.Name)
	}

	role := RelayRole(in)
	if role.Namespace != in.Namespace {
		t.Fatalf("Role namespace = %q, want namespaced to the Instance", role.Namespace)
	}
	if len(role.Rules) != 2 {
		t.Fatalf("rules = %d, want pod rule plus own-instance status rule", len(role.Rules))
	}
	statusRule := role.Rules[1]
	if statusRule.APIGroups[0] != "typeclaw.fml09.io" ||
		statusRule.Resources[0] != "typeclawinstances/status" ||
		statusRule.ResourceNames[0] != in.Name {
		t.Fatalf("status rule must be restricted to this Instance's status subresource: %+v", statusRule)
	}
	rule := role.Rules[0]
	if rule.APIGroups[0] != "" || rule.Resources[0] != "pods" {
		t.Fatalf("rule targets %v/%v, want core pods", rule.APIGroups, rule.Resources)
	}
	wantNames := []string{"kakao-agent-0"}
	for i, n := range rule.ResourceNames {
		if n != wantNames[i] {
			t.Fatalf("resourceNames = %v, want %v", rule.ResourceNames, wantNames)
		}
	}
	verbs := map[string]bool{}
	for _, v := range rule.Verbs {
		verbs[v] = true
	}
	if !verbs["get"] || !verbs["delete"] || len(verbs) != 2 {
		t.Fatalf("verbs = %v, want exactly get+delete", rule.Verbs)
	}

	binding := RelayRoleBinding(in)
	if binding.RoleRef != (rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "Role",
		Name:     "kakao-agent-relay",
	}) {
		t.Fatalf("roleRef = %+v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 ||
		binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Name != "kakao-agent-relay" ||
		binding.Subjects[0].Namespace != in.Namespace {
		t.Fatalf("subjects = %+v", binding.Subjects)
	}
}
