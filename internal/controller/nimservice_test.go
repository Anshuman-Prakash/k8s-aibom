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

func summaryModelIdentities(a *aibomv1beta1.AIBOM) []string {
	var out []string
	for _, m := range a.Status.Summary.Models {
		out = append(out, m.Identity)
	}
	return out
}

func waitForAIBOM(t *testing.T, env *envTestEnv, key types.NamespacedName) aibomv1beta1.AIBOM {
	t.Helper()
	var got aibomv1beta1.AIBOM
	eventually(t, 30*time.Second, 200*time.Millisecond, func() error {
		if err := env.k8sClient.Get(context.Background(), key, &got); err != nil {
			return fmt.Errorf("get AIBOM %s: %w", key, err)
		}
		if got.Status.Summary == nil {
			return errors.New("status.summary not yet populated")
		}
		return nil
	})
	return got
}

// The canonical NIM deployment (nvcr.io/nim image + nimCache) produces
// one AIBOM keyed to the NIMService: runtime nim declared, the model
// derived from the image path, the NIMService as owner.
func TestIntegration_NIMService_ProducesAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "prod-nim"
	svcName := "llama-nim"
	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	u := nimServiceCR(nsName, svcName, "nvcr.io/nim/meta/llama-3.1-8b-instruct", "1.3.0")
	_ = unstructured.SetNestedField(u.Object, "llama-cache", "spec", "storage", "nimCache", "name")
	mustCreate(t, env.k8sClient, ctx, u)

	got := waitForAIBOM(t, env, types.NamespacedName{
		Name:      AIBOMNameForWorkload("apps.nvidia.com", "NIMService", svcName),
		Namespace: nsName,
	})

	if got.Spec.WorkloadRef.Kind != "NIMService" || got.Spec.WorkloadRef.APIVersion != "apps.nvidia.com/v1alpha1" || got.Spec.WorkloadRef.Name != svcName {
		t.Errorf("WorkloadRef = %+v", got.Spec.WorkloadRef)
	}
	if got.Status.Summary.Runtime == nil || got.Status.Summary.Runtime.Name != "nim" || got.Status.Summary.Runtime.Confidence != "declared" {
		t.Errorf("Summary.Runtime = %+v, want nim/declared", got.Status.Summary.Runtime)
	}
	models := summaryModelIdentities(&got)
	if len(models) != 1 || models[0] != "meta/llama-3.1-8b-instruct" {
		t.Errorf("summary models = %v, want [meta/llama-3.1-8b-instruct]", models)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "NIMService" || got.OwnerReferences[0].Name != svcName {
		t.Errorf("owner references = %+v", got.OwnerReferences)
	}
}

// Declared NIM_MODEL_NAME wins over the image path end to end (the AICR
// case: a llama NIM image serving Qwen).
func TestIntegration_NIMService_DeclaredEnvWins(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "prod-nim-env"
	svcName := "qwen-on-llama-nim"
	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	u := nimServiceCR(nsName, svcName, "nvcr.io/nim/meta/llama-3.1-8b-instruct", "1.3.0")
	_ = unstructured.SetNestedSlice(u.Object, []interface{}{
		map[string]interface{}{"name": "NIM_MODEL_NAME", "value": "Qwen/Qwen3-0.6B"},
	}, "spec", "env")
	mustCreate(t, env.k8sClient, ctx, u)

	got := waitForAIBOM(t, env, types.NamespacedName{
		Name:      AIBOMNameForWorkload("apps.nvidia.com", "NIMService", svcName),
		Namespace: nsName,
	})
	models := summaryModelIdentities(&got)
	if len(models) != 1 || models[0] != "Qwen/Qwen3-0.6B" {
		t.Errorf("summary models = %v, want only the declared Qwen/Qwen3-0.6B", models)
	}
}

// Opt-in applies to NIMService exactly as to every other kind.
func TestIntegration_NIMService_NotOptedIn_NoAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "plain-nim"
	svcName := "llama-plain"
	mustCreate(t, env.k8sClient, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
	mustCreate(t, env.k8sClient, ctx, nimServiceCR(nsName, svcName, "nvcr.io/nim/meta/llama-3.1-8b-instruct", "1.3.0"))

	time.Sleep(500 * time.Millisecond)
	var aibom aibomv1beta1.AIBOM
	key := types.NamespacedName{Name: AIBOMNameForWorkload("apps.nvidia.com", "NIMService", svcName), Namespace: nsName}
	if err := env.k8sClient.Get(ctx, key, &aibom); err == nil {
		t.Fatalf("AIBOM unexpectedly created in a namespace that is not opted in: %+v", aibom)
	}
}

// nimServiceCR returns an unstructured NIMService with the two fields
// the real CRD requires (image.repository, image.tag).
func nimServiceCR(namespace, name, repo, tag string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apps.nvidia.com/v1alpha1")
	u.SetKind("NIMService")
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedField(u.Object, repo, "spec", "image", "repository")
	_ = unstructured.SetNestedField(u.Object, tag, "spec", "image", "tag")
	return u
}
