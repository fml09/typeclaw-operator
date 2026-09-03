/*
Copyright 2026 fml09.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"os"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	generatedRolePath = "../../config/rbac/role.yaml"
	chartRolePath     = "../../charts/typeclaw-operator/templates/clusterrole.yaml"
)

// chartRoleRules reads a ClusterRole document, dropping Helm template actions
// first. The chart's rules carry no template directives, so removing the lines
// that do leaves a plain YAML document the API types can decode.
func chartRoleRules(t *testing.T, path string) []rbacv1.PolicyRule {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "{{") {
			continue
		}
		kept = append(kept, line)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &role); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(role.Rules) == 0 {
		t.Fatalf("%s declares no rules", path)
	}
	return role.Rules
}

// grantSet flattens rules into one comparable grant per
// (apiGroup, resource, resourceName, verb). controller-gen merges rules by
// verb set while the chart is written by hand, so two equivalent files can
// have entirely different rule shapes; only the grants they add up to matter.
func grantSet(rules []rbacv1.PolicyRule) []string {
	var grants []string
	for _, rule := range rules {
		names := rule.ResourceNames
		if len(names) == 0 {
			names = []string{""}
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, name := range names {
					for _, verb := range rule.Verbs {
						grants = append(grants, strings.Join([]string{group, resource, name, verb}, "|"))
					}
				}
			}
		}
	}
	sort.Strings(grants)
	return grants
}

// TestChartClusterRoleMatchesGeneratedRole is the only thing keeping the chart
// honest: controller-gen writes config/rbac/role.yaml from the kubebuilder
// markers and never touches the chart, so a new marker silently ships an
// operator whose Helm release cannot do what its code assumes.
func TestChartClusterRoleMatchesGeneratedRole(t *testing.T) {
	generated := grantSet(chartRoleRules(t, generatedRolePath))
	chart := grantSet(chartRoleRules(t, chartRolePath))

	missing := difference(generated, chart)
	extra := difference(chart, generated)
	if len(missing) > 0 {
		t.Errorf("the chart ClusterRole is missing %d grant(s) from %s:\n  %s",
			len(missing), generatedRolePath, strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("the chart ClusterRole grants %d rule(s) the manager never asks for:\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

func difference(left, right []string) []string {
	present := make(map[string]struct{}, len(right))
	for _, value := range right {
		present[value] = struct{}{}
	}
	var only []string
	for _, value := range left {
		if _, found := present[value]; !found {
			only = append(only, value)
		}
	}
	return only
}
