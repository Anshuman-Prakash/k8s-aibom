/*
Copyright 2026 Google LLC

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
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	aibomv1beta1 "github.com/GoogleCloudPlatform/k8s-aibom/api/v1beta1"
)

// TestIntegration_DynamoGraphDeployment_ProducesAIBOM is the Dynamo
// equivalent of the KServe happy path: a DynamoGraphDeployment with a
// declared backendFramework, a frontend component and a worker
// component carrying modelRef produces one AIBOM keyed to the DGD with
//   - Workload.Kind = "DynamoGraphDeployment", APIVersion nvidia.com/v1beta1
//   - Summary.Runtime.Name = the mapped backend (vllm), Confidence declared
//     (the declared enum sorts ahead of the image-inferred attribution)
//   - the modelRef model in the summary
//   - an owner reference to the DGD
func TestIntegration_DynamoGraphDeployment_ProducesAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "prod-dynamo"
	dgdName := "qwen-agg"

	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	mustCreate(t, env.k8sClient, ctx, dynamoGraphDeployment(nsName, dgdName, "vllm",
		dynamoComponent("Frontend", "frontend", "", "nvcr.io/nvidia/ai-dynamo/dynamo-frontend:0.6.0"),
		dynamoComponent("VllmWorker", "worker", "Qwen/Qwen3-0.6B", "nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.6.0"),
	))

	aibomKey := types.NamespacedName{
		Name:      AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", dgdName),
		Namespace: nsName,
	}
	var got aibomv1beta1.AIBOM
	eventually(t, 30*time.Second, 200*time.Millisecond, func() error {
		if err := env.k8sClient.Get(ctx, aibomKey, &got); err != nil {
			return fmt.Errorf("get AIBOM %s: %w", aibomKey, err)
		}
		if got.Status.Summary == nil {
			return errors.New("status.summary not yet populated")
		}
		return nil
	})

	if got.Spec.WorkloadRef.Kind != "DynamoGraphDeployment" {
		t.Errorf("Spec.WorkloadRef.Kind = %q, want DynamoGraphDeployment", got.Spec.WorkloadRef.Kind)
	}
	if got.Spec.WorkloadRef.APIVersion != "nvidia.com/v1beta1" {
		t.Errorf("Spec.WorkloadRef.APIVersion = %q, want nvidia.com/v1beta1", got.Spec.WorkloadRef.APIVersion)
	}
	if got.Spec.WorkloadRef.Name != dgdName {
		t.Errorf("Spec.WorkloadRef.Name = %q, want %q", got.Spec.WorkloadRef.Name, dgdName)
	}
	if got.Status.Summary.Workload.Kind != "DynamoGraphDeployment" {
		t.Errorf("Summary.Workload.Kind = %q, want DynamoGraphDeployment", got.Status.Summary.Workload.Kind)
	}
	if got.Status.Summary.Runtime == nil {
		t.Fatal("Summary.Runtime is nil; expected vllm from declared backendFramework")
	}
	if got.Status.Summary.Runtime.Name != "vllm" {
		t.Errorf("Runtime.Name = %q, want vllm", got.Status.Summary.Runtime.Name)
	}
	if got.Status.Summary.Runtime.Confidence != "declared" {
		t.Errorf("Runtime.Confidence = %q, want declared (spec.backendFramework is customer-declared and API-validated)",
			got.Status.Summary.Runtime.Confidence)
	}
	foundModel := false
	for _, m := range got.Status.Summary.Models {
		if m.Identity == "Qwen/Qwen3-0.6B" {
			foundModel = true
		}
	}
	if !foundModel {
		t.Errorf("summary models = %+v, want Qwen/Qwen3-0.6B from spec.components[1].modelRef", got.Status.Summary.Models)
	}
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(got.OwnerReferences))
	}
	if owner := got.OwnerReferences[0]; owner.Kind != "DynamoGraphDeployment" || owner.Name != dgdName {
		t.Errorf("owner reference = %+v, want DynamoGraphDeployment/%s", owner, dgdName)
	}
}

// TestIntegration_DynamoGraphDeployment_NoSignal_NoAIBOM: a DGD with no
// backendFramework, no components and no annotations produces no
// AIBOM — the scraper returns unresolved and the reconciler declines.
func TestIntegration_DynamoGraphDeployment_NoSignal_NoAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "prod-dynamo-empty"
	dgdName := "empty-dgd"

	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	mustCreate(t, env.k8sClient, ctx, minimalUnstructuredDGD(nsName, dgdName))

	time.Sleep(500 * time.Millisecond)
	aibomKey := types.NamespacedName{
		Name:      AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", dgdName),
		Namespace: nsName,
	}
	var aibom aibomv1beta1.AIBOM
	if err := env.k8sClient.Get(ctx, aibomKey, &aibom); err == nil {
		t.Fatalf("AIBOM unexpectedly created for empty DynamoGraphDeployment: %+v", aibom)
	}
}

// TestIntegration_DynamoGraphDeployment_NotOptedIn_NoAIBOM: the opt-in
// rule applies to CRD kinds exactly as to apps/v1 kinds.
func TestIntegration_DynamoGraphDeployment_NotOptedIn_NoAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "plain-dynamo"
	dgdName := "qwen-plain"

	mustCreate(t, env.k8sClient, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
	mustCreate(t, env.k8sClient, ctx, dynamoGraphDeployment(nsName, dgdName, "sglang",
		dynamoComponent("Worker", "worker", "Qwen/Qwen3-0.6B", "nvcr.io/nvidia/ai-dynamo/sglang-runtime:0.6.0"),
	))

	time.Sleep(500 * time.Millisecond)
	aibomKey := types.NamespacedName{
		Name:      AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", dgdName),
		Namespace: nsName,
	}
	var aibom aibomv1beta1.AIBOM
	if err := env.k8sClient.Get(ctx, aibomKey, &aibom); err == nil {
		t.Fatalf("AIBOM unexpectedly created in a namespace that is not opted in: %+v", aibom)
	}
}

// dynamoComponent builds one spec.components[] entry. modelRef is
// omitted when model is empty.
func dynamoComponent(name, ctype, model, image string) map[string]interface{} {
	c := map[string]interface{}{
		"name": name,
		"type": ctype,
		"podTemplate": map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "main", "image": image},
				},
			},
		},
	}
	if model != "" {
		c["modelRef"] = map[string]interface{}{"name": model}
	}
	return c
}

// dynamoGraphDeployment returns a populated DynamoGraphDeployment
// *unstructured.Unstructured ready for creation in envtest.
func dynamoGraphDeployment(namespace, name, backend string, components ...map[string]interface{}) *unstructured.Unstructured {
	u := minimalUnstructuredDGD(namespace, name)
	_ = unstructured.SetNestedField(u.Object, backend, "spec", "backendFramework")
	comps := make([]interface{}, 0, len(components))
	for _, c := range components {
		comps = append(comps, c)
	}
	_ = unstructured.SetNestedSlice(u.Object, comps, "spec", "components")
	return u
}

// minimalUnstructuredDGD returns a bare DynamoGraphDeployment with an
// empty spec. Used for the no-signal test.
func minimalUnstructuredDGD(namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("nvidia.com/v1beta1")
	u.SetKind("DynamoGraphDeployment")
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{}, "spec")
	return u
}
