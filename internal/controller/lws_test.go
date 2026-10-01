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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	aibomv1beta1 "github.com/GoogleCloudPlatform/k8s-aibom/api/v1beta1"
)

// A multi-host vLLM LeaderWorkerSet (vllm image + --model in both
// templates) produces one AIBOM keyed to the LWS: runtime vllm inferred
// from the image pattern, the model from the args, the LWS as owner.
func TestIntegration_LeaderWorkerSet_ProducesAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "prod-lws"
	lwsName := "qwen-tp8"
	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	tmpl := lwsPodTemplate("vllm/vllm-openai:v0.6.3", "--model", "Qwen/Qwen3-32B")
	mustCreate(t, env.k8sClient, ctx, leaderWorkerSetCR(nsName, lwsName, tmpl, tmpl, 4))

	got := waitForAIBOM(t, env, types.NamespacedName{
		Name:      AIBOMNameForWorkload("leaderworkerset.x-k8s.io", "LeaderWorkerSet", lwsName),
		Namespace: nsName,
	})

	if got.Spec.WorkloadRef.Kind != "LeaderWorkerSet" || got.Spec.WorkloadRef.APIVersion != "leaderworkerset.x-k8s.io/v1" || got.Spec.WorkloadRef.Name != lwsName {
		t.Errorf("WorkloadRef = %+v", got.Spec.WorkloadRef)
	}
	if got.Status.Summary.Runtime == nil || got.Status.Summary.Runtime.Name != "vllm" || got.Status.Summary.Runtime.Confidence != "inferred" {
		t.Errorf("Summary.Runtime = %+v, want vllm/inferred (image pattern)", got.Status.Summary.Runtime)
	}
	found := false
	for _, id := range summaryModelIdentities(&got) {
		if id == "Qwen/Qwen3-32B" {
			found = true
		}
	}
	if !found {
		t.Errorf("summary models = %v, want Qwen/Qwen3-32B", summaryModelIdentities(&got))
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "LeaderWorkerSet" || got.OwnerReferences[0].Name != lwsName {
		t.Errorf("owner references = %+v", got.OwnerReferences)
	}
}

// Worker-only signal (leader template absent) still produces the AIBOM.
func TestIntegration_LeaderWorkerSet_WorkerOnly(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "prod-lws-worker"
	lwsName := "sglang-workers"
	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	mustCreate(t, env.k8sClient, ctx, leaderWorkerSetCR(nsName, lwsName, nil,
		lwsPodTemplate("lmsysorg/sglang:v0.4.0", "--model-path", "Qwen/Qwen3-0.6B"), 2))

	got := waitForAIBOM(t, env, types.NamespacedName{
		Name:      AIBOMNameForWorkload("leaderworkerset.x-k8s.io", "LeaderWorkerSet", lwsName),
		Namespace: nsName,
	})
	if got.Status.Summary.Runtime == nil || got.Status.Summary.Runtime.Name != "sglang" {
		t.Errorf("Summary.Runtime = %+v, want sglang", got.Status.Summary.Runtime)
	}
}

// Opt-in applies to LeaderWorkerSet exactly as to every other kind.
func TestIntegration_LeaderWorkerSet_NotOptedIn_NoAIBOM(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	nsName := "plain-lws"
	lwsName := "qwen-plain"
	mustCreate(t, env.k8sClient, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})
	tmpl := lwsPodTemplate("vllm/vllm-openai:v0.6.3", "--model", "Qwen/Qwen3-32B")
	mustCreate(t, env.k8sClient, ctx, leaderWorkerSetCR(nsName, lwsName, tmpl, tmpl, 2))

	time.Sleep(500 * time.Millisecond)
	var aibom aibomv1beta1.AIBOM
	key := types.NamespacedName{Name: AIBOMNameForWorkload("leaderworkerset.x-k8s.io", "LeaderWorkerSet", lwsName), Namespace: nsName}
	if err := env.k8sClient.Get(ctx, key, &aibom); err == nil {
		t.Fatalf("AIBOM unexpectedly created in a namespace that is not opted in: %+v", aibom)
	}
}

func lwsPodTemplate(image string, args ...string) map[string]interface{} {
	a := make([]interface{}, 0, len(args))
	for _, s := range args {
		a = append(a, s)
	}
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "main", "image": image, "args": a},
			},
		},
	}
}

// leaderWorkerSetCR returns an unstructured LeaderWorkerSet. leader may
// be nil (absent); worker is required upstream.
func leaderWorkerSetCR(namespace, name string, leader, worker map[string]interface{}, size int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
	u.SetKind("LeaderWorkerSet")
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedMap(u.Object, worker, "spec", "leaderWorkerTemplate", "workerTemplate")
	if leader != nil {
		_ = unstructured.SetNestedMap(u.Object, leader, "spec", "leaderWorkerTemplate", "leaderTemplate")
	}
	_ = unstructured.SetNestedField(u.Object, size, "spec", "leaderWorkerTemplate", "size")
	return u
}
