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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GoogleCloudPlatform/k8s-aibom/internal/metrics"
)

// The reconcile-outcome counter is the signal that separates "opted-in
// namespace, nothing recognized" from a controller that is not running
// (#106). Each outcome must be observable for a workload that reaches
// it. Counters are process-global across the package's envtests, so
// every assertion is a delta against a snapshot taken before the
// scenario is created.
func TestIntegration_ReconcileOutcomeMetrics(t *testing.T) {
	env := startEnvTest(t)
	ctx := context.Background()

	outcome := func(o string) float64 {
		return testutil.ToFloat64(metrics.WorkloadReconcileOutcomes.WithLabelValues("Deployment", o))
	}
	before := map[string]float64{
		"not_opted_in": outcome("not_opted_in"),
		"unmatched":    outcome("unmatched"),
		"matched":      outcome("matched"),
	}

	// args are passed explicitly: a declared --model flag is a model
	// claim regardless of image, so the unmatched fixture must carry
	// no inference signal of any kind — plain image, no args.
	dep := func(ns, name, image string, args ...string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: image, Args: args}},
					},
				},
			},
		}
	}

	// (a) not opted in: an inference image in a namespace without the
	// label must count as not_opted_in, never as matched.
	mustCreate(t, env.k8sClient, ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "outcomes-plain"}})
	mustCreate(t, env.k8sClient, ctx, dep("outcomes-plain", "vllm-a", "vllm/vllm-openai:v0.6.3", "--model", "facebook/opt-125m"))

	// (b) opted in, no inference signal: the case Mark hit — visibly
	// "seen and declined".
	mustCreateOptedInNamespace(t, env.k8sClient, ctx, "outcomes-optin")
	mustCreate(t, env.k8sClient, ctx, dep("outcomes-optin", "nginx", "nginx:1.27"))

	// (c) opted in, inference signal: matched.
	mustCreate(t, env.k8sClient, ctx, dep("outcomes-optin", "vllm-b", "vllm/vllm-openai:v0.6.3", "--model", "facebook/opt-125m"))

	eventually(t, 30*time.Second, 200*time.Millisecond, func() error {
		for _, o := range []string{"not_opted_in", "unmatched", "matched"} {
			if outcome(o) <= before[o] {
				return errors.New("outcome not yet counted: " + o)
			}
		}
		return nil
	})
}
