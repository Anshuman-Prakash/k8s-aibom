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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aibomv1beta1 "github.com/GoogleCloudPlatform/k8s-aibom/api/v1beta1"
	"github.com/GoogleCloudPlatform/k8s-aibom/internal/sink"
)

// Regression test for the sink retry gap: a transiently failed sink
// must be retried until delivery succeeds, and the archive must heal.
// Pre-fix behavior (confirmed by a failing version of this test):
// InputHash was persisted despite the sink error, the next reconcile
// took the dedup fast path and returned before emitting, and the BOM
// stayed absent from the archive until the workload spec changed.
//
// The fix's contract, asserted here:
//  1. While a sink is failing, InputHash is NOT persisted (dedup must
//     not swallow the re-emit) and SinkFailed=True.
//  2. The reconcile requeues on a bounded cadence, so the retry needs
//     no external event.
//  3. When the sink heals, a subsequent emit succeeds, SinkFailed
//     clears, and InputHash is persisted again (dedup restored).
func TestIntegration_SinkFailure_RetriedUntilArchiveHeals(t *testing.T) {
	prev := SinkRetryRequeueAfter
	SinkRetryRequeueAfter = 2 * time.Second
	t.Cleanup(func() { SinkRetryRequeueAfter = prev })

	failErr := errors.New("simulated transient 403")
	rs := &recordingSink{name: "flaky-archive", err: failErr}
	env := startEnvTestWithSinks(t, []sink.Sink{rs})
	ctx := context.Background()

	nsName := "sink-retry"
	depName := "vllm-retry"
	mustCreateOptedInNamespace(t, env.k8sClient, ctx, nsName)
	mustCreate(t, env.k8sClient, ctx, vllmDeployment(nsName, depName))

	aibomKey := types.NamespacedName{
		Name: "apps-deployment-" + depName, Namespace: nsName,
	}

	// Phase 1: failure recorded; the fix's signature is InputHash
	// staying empty while SinkFailed is True.
	var got aibomv1beta1.AIBOM
	eventually(t, 30*time.Second, 200*time.Millisecond, func() error {
		if err := env.k8sClient.Get(ctx, aibomKey, &got); err != nil {
			return err
		}
		conds := conditionsByType(got.Status.Conditions)
		if conds[aibomv1beta1.ConditionSinkFailed].Status != metav1.ConditionTrue {
			return errors.New("SinkFailed not yet True")
		}
		if rs.emitCount() == 0 {
			return errors.New("sink not yet attempted")
		}
		return nil
	})
	if got.Status.InputHash != "" {
		t.Fatalf("InputHash = %q persisted while a sink is failing; the dedup fast path would swallow the retry", got.Status.InputHash)
	}

	// Phase 2: heal the sink. No workload change, no manual poke —
	// the bounded requeue (shrunk above) must drive the retry on its
	// own.
	attemptsBeforeHeal := rs.emitCount()
	rs.mu.Lock()
	rs.err = nil
	rs.url = "gs://healed-bucket/object.json"
	rs.mu.Unlock()

	eventually(t, 30*time.Second, 500*time.Millisecond, func() error {
		if rs.emitCount() <= attemptsBeforeHeal {
			return errors.New("no retry attempted yet")
		}
		if err := env.k8sClient.Get(ctx, aibomKey, &got); err != nil {
			return err
		}
		conds := conditionsByType(got.Status.Conditions)
		if conds[aibomv1beta1.ConditionSinkFailed].Status != metav1.ConditionFalse {
			return errors.New("SinkFailed still True after heal")
		}
		if got.Status.InputHash == "" {
			return errors.New("InputHash not yet re-persisted after successful emit")
		}
		return nil
	})
}
