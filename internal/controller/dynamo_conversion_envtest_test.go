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
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	aibomv1beta1 "github.com/GoogleCloudPlatform/k8s-aibom/api/v1beta1"
	"github.com/GoogleCloudPlatform/k8s-aibom/internal/bom"
	"github.com/GoogleCloudPlatform/k8s-aibom/internal/config"
	"github.com/GoogleCloudPlatform/k8s-aibom/internal/scraper"
)

// dgdTwoVersionCRD mirrors the deployed Dynamo shape (#127): v1alpha1 is
// the storage version and v1beta1 is served. envtest strips conversion
// webhooks for kinds its scheme does not know, so the webhook is added
// to the live CRD by the test (see setDGDConversion).
const dgdTwoVersionCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: dynamographdeployments.nvidia.com
spec:
  group: nvidia.com
  scope: Namespaced
  names:
    plural: dynamographdeployments
    singular: dynamographdeployment
    kind: DynamoGraphDeployment
    listKind: DynamoGraphDeploymentList
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
    - name: v1beta1
      served: true
      storage: false
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
`

type webhookExperiment struct {
	cfgClient client.Client
	mgr       ctrl.Manager
	ctx       context.Context
}

// setDGDConversion points the live CRD's conversion at an unreachable
// webhook (down=true), which is what a stopped or uninstalled Dynamo
// operator looks like to the API server, or back to None (down=false).
func setDGDConversion(t *testing.T, c client.Client, ctx context.Context, down bool) {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	if err := c.Get(ctx, types.NamespacedName{Name: "dynamographdeployments.nvidia.com"}, crd); err != nil {
		t.Fatal(err)
	}
	conv := map[string]interface{}{"strategy": "None"}
	if down {
		conv = map[string]interface{}{
			"strategy": "Webhook",
			"webhook": map[string]interface{}{
				"conversionReviewVersions": []interface{}{"v1"},
				"clientConfig":             map[string]interface{}{"url": "https://127.0.0.1:1/convert"},
			},
		}
	}
	_ = unstructured.SetNestedMap(crd.Object, conv, "spec", "conversion")
	if err := c.Update(ctx, crd); err != nil {
		t.Fatalf("updating CRD conversion: %v", err)
	}
	time.Sleep(1 * time.Second)
}

func newWebhookExperiment(t *testing.T) *webhookExperiment {
	t.Helper()
	// Investigation record for #127 / Design 004, kept reproducible.
	// These runs log observations rather than asserting a contract and
	// take ~90 s; the asserting regression suite lands with the fix.
	if os.Getenv("AIBOM_WATCH_EXPERIMENT") == "" {
		t.Skip("set AIBOM_WATCH_EXPERIMENT=1 to run the Design 004 investigation experiments")
	}
	t.Setenv("AIBOM_DISABLE_SSRF_CHECKS", "true")
	crdDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(crdDir, "dgd.yaml"), []byte(dgdTwoVersionCRD), 0o600); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(aibomv1beta1.AddToScheme(scheme))
	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases"), crdDir},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := te.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = te.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Controller: ctrlcfg.Controller{
			SkipNameValidation: ptr.To(true),
			CacheSyncTimeout:   8 * time.Second, // experiment: seconds, not the 2-minute default
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := WorkloadReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Scraper:           scraper.NewInferenceSpecScraper(nil),
		BOMBuilder:        bom.NewBuilder(),
		StatusBuilder:     NewStatusBuilder(),
		ConfigStore:       config.NewStore(config.DefaultSnapshot()),
		ControllerName:    "k8s-aibom",
		ControllerVersion: "0.1.0-test",
	}
	dynamoBase := base
	dynamoBase.Scraper = scraper.NewDynamoGraphDeploymentScraper(nil)
	if err := (&DeploymentReconciler{WorkloadReconciler: base}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err := (&DynamoGraphDeploymentReconciler{WorkloadReconciler: dynamoBase}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	return &webhookExperiment{cfgClient: c, mgr: mgr, ctx: context.Background()}
}

func expVllmDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "c", Image: "vllm/vllm-openai:v0.6.3", Args: []string{"--model", "facebook/opt-125m"},
				}}},
			},
		},
	}
}

func dgdAt(version, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("nvidia.com/" + version)
	u.SetKind("DynamoGraphDeployment")
	u.SetName(name)
	u.SetNamespace(ns)
	_ = unstructured.SetNestedField(u.Object, "vllm", "spec", "backendFramework")
	_ = unstructured.SetNestedSlice(u.Object, []interface{}{
		map[string]interface{}{"name": "W", "type": "worker",
			"modelRef": map[string]interface{}{"name": "Qwen/Qwen3-0.6B"}},
	}, "spec", "components")
	return u
}

func aibomExists(c client.Client, ctx context.Context, key types.NamespacedName) bool {
	var a aibomv1beta1.AIBOM
	return c.Get(ctx, key, &a) == nil && a.Status.Summary != nil
}

// observation is what startAndObserve saw within its window.
type observation struct {
	deployAIBOM bool  // the unrelated Deployment got an AIBOM
	exited      bool  // the manager returned from Start
	err         error // what Start returned, when exited
}

// startAndObserve starts the manager and reports whether the Deployment
// AIBOM appeared and whether/why the manager exited within window. The
// returned cancel stops the manager if it is still running.
func (e *webhookExperiment) startAndObserve(t *testing.T, ns string, window time.Duration) (observation, func()) {
	t.Helper()
	mgrCtx, c := context.WithCancel(e.ctx)
	errCh := make(chan error, 1)
	start := time.Now()
	go func() { errCh <- e.mgr.Start(mgrCtx) }()
	deployKey := types.NamespacedName{Name: AIBOMNameForWorkload("apps", "Deployment", "vllm"), Namespace: ns}
	deadline := time.After(window)
	var obs observation
	for {
		select {
		case err := <-errCh:
			t.Logf("manager exited after %s: %v", time.Since(start).Round(100*time.Millisecond), err)
			obs.exited, obs.err = true, err
			return obs, c
		case <-deadline:
			return obs, func() { c(); <-errCh }
		case <-time.After(250 * time.Millisecond):
			if !obs.deployAIBOM && aibomExists(e.cfgClient, e.ctx, deployKey) {
				obs.deployAIBOM = true
				t.Logf("Deployment AIBOM created after %s", time.Since(start).Round(100*time.Millisecond))
			}
		}
	}
}

// Control: webhook healthy (strategy None). Establishes that the 8s
// cache-sync budget is enough and both reconcilers work.
func TestIntegration_DynamoConversion_Control(t *testing.T) {
	e := newWebhookExperiment(t)
	ns := "dyn-ctl"
	mustCreateOptedInNamespace(t, e.cfgClient, e.ctx, ns)
	mustCreate(t, e.cfgClient, e.ctx, dgdAt("v1alpha1", ns, "stored-alpha"))
	mustCreate(t, e.cfgClient, e.ctx, expVllmDeployment(ns, "vllm"))

	obs, cancel := e.startAndObserve(t, ns, 20*time.Second)
	defer cancel()
	dgdOK := aibomExists(e.cfgClient, e.ctx, types.NamespacedName{Name: AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", "stored-alpha"), Namespace: ns})
	t.Logf("RESULT control: managerExited=%v err=%v deploymentAIBOM=%v dgdAIBOM(from stored v1alpha1, converted by None)=%v", obs.exited, obs.err, obs.deployAIBOM, dgdOK)
	if obs.exited || !obs.deployAIBOM || !dgdOK {
		t.Errorf("control must be healthy: exited=%v deploy=%v dgd=%v", obs.exited, obs.deployAIBOM, dgdOK)
	}
}

// Startup case: webhook unreachable before the manager starts.
func TestIntegration_DynamoConversion_WebhookDownAtStartup(t *testing.T) {
	e := newWebhookExperiment(t)
	ns := "dyn-down"
	mustCreateOptedInNamespace(t, e.cfgClient, e.ctx, ns)
	mustCreate(t, e.cfgClient, e.ctx, dgdAt("v1alpha1", ns, "stored-alpha"))
	mustCreate(t, e.cfgClient, e.ctx, expVllmDeployment(ns, "vllm"))
	setDGDConversion(t, e.cfgClient, e.ctx, true)

	beta := &unstructured.UnstructuredList{}
	beta.SetAPIVersion("nvidia.com/v1beta1")
	beta.SetKind("DynamoGraphDeploymentList")
	err := e.cfgClient.List(e.ctx, beta, client.InNamespace(ns))
	t.Logf("direct v1beta1 list: err=%v", err)

	obs, cancel := e.startAndObserve(t, ns, 30*time.Second)
	defer cancel()
	t.Logf("RESULT webhook-down-at-startup: managerExited=%v err=%v deploymentAIBOM=%v", obs.exited, obs.err, obs.deployAIBOM)
}

// Runtime case: healthy start, then the webhook dies and a new DGD is
// created at the storage version. Does the manager stay up? Does the
// new object get an AIBOM? Do existing Dynamo AIBOMs survive? Is
// anything surfaced beyond logs?
func TestIntegration_DynamoConversion_WebhookDiesAfterStartup(t *testing.T) {
	e := newWebhookExperiment(t)
	ns := "dyn-late"
	mustCreateOptedInNamespace(t, e.cfgClient, e.ctx, ns)
	mustCreate(t, e.cfgClient, e.ctx, dgdAt("v1alpha1", ns, "early"))
	mustCreate(t, e.cfgClient, e.ctx, expVllmDeployment(ns, "vllm"))

	obs, cancel := e.startAndObserve(t, ns, 15*time.Second)
	defer cancel()
	earlyKey := types.NamespacedName{Name: AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", "early"), Namespace: ns}
	if obs.exited || !obs.deployAIBOM || !aibomExists(e.cfgClient, e.ctx, earlyKey) {
		t.Fatalf("healthy phase failed: exited=%v deploy=%v early=%v", obs.exited, obs.deployAIBOM, aibomExists(e.cfgClient, e.ctx, earlyKey))
	}

	// Webhook dies; a new graph appears at the storage version.
	setDGDConversion(t, e.cfgClient, e.ctx, true)
	mustCreate(t, e.cfgClient, e.ctx, dgdAt("v1alpha1", ns, "late"))
	// Also a new unrelated Deployment, to see whether the apps path still works.
	mustCreate(t, e.cfgClient, e.ctx, expVllmDeployment(ns, "vllm2"))

	lateKey := types.NamespacedName{Name: AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", "late"), Namespace: ns}
	deploy2Key := types.NamespacedName{Name: AIBOMNameForWorkload("apps", "Deployment", "vllm2"), Namespace: ns}
	time.Sleep(20 * time.Second)
	events := &corev1.EventList{}
	_ = e.cfgClient.List(e.ctx, events, client.InNamespace(ns))
	warn := 0
	for _, ev := range events.Items {
		if ev.Type == corev1.EventTypeWarning {
			warn++
			t.Logf("warning event: %s/%s %s: %s", ev.InvolvedObject.Kind, ev.InvolvedObject.Name, ev.Reason, ev.Message)
		}
	}
	t.Logf("RESULT webhook-dies-after-startup: lateDGD AIBOM=%v earlyDGD AIBOM still present=%v newDeployment AIBOM=%v warningEvents=%d",
		aibomExists(e.cfgClient, e.ctx, lateKey), aibomExists(e.cfgClient, e.ctx, earlyKey), aibomExists(e.cfgClient, e.ctx, deploy2Key), warn)
}
