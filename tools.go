//go:build tools

// Package tools pins code-generation binaries to versions resolved by this
// module's go.mod instead of floating @latest invocations.
package tools

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)
