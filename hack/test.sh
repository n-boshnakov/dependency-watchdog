#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

set -e

echo "> test"

ENVTEST_K8S_VERSION="1.31"
export KUBEBUILDER_ASSETS="$(setup-envtest --os $(go env GOOS) --arch $(go env GOARCH) use $ENVTEST_K8S_VERSION -p path)"
echo "Running tests using KUBEBUILDER_ASSETS=$KUBEBUILDER_ASSETS"
export KUBEBUILDER_ATTACH_CONTROL_PLANE_OUTPUT=true
# Tests using envtest needs to be serialized as there are issues in starting more than one envtest concurrently.
# see https://github.com/kubernetes-sigs/controller-runtime/issues/1363 which remains unresolved.
go test -json -cover ./controllers/cluster | gotestfmt -hide empty-packages
go test -json -cover ./controllers/endpoint | gotestfmt -hide empty-packages
go test -json -cover `go list ./internal/... | grep -v fakes | grep -v test` | gotestfmt -hide empty-packages
