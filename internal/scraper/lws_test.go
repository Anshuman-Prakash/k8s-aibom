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

package scraper

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var fixedLWSTime = time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)

var lwsKind = WorkloadKind{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"}

func newLWSScraper() *LeaderWorkerSetScraper {
	s := NewLeaderWorkerSetScraper(nil)
	s.now = func() time.Time { return fixedLWSTime }
	return s
}

// podTemplate builds an unstructured PodTemplateSpec with one container.
func podTemplate(image string, args ...string) map[string]interface{} {
	c := map[string]interface{}{"name": "main", "image": image}
	if len(args) > 0 {
		a := make([]interface{}, 0, len(args))
		for _, s := range args {
			a = append(a, s)
		}
		c["args"] = a
	}
	return map[string]interface{}{
		"spec": map[string]interface{}{"containers": []interface{}{c}},
	}
}

// lws returns an unstructured LeaderWorkerSet with the given worker
// template (required upstream) and optional leader template (nil = absent).
func lws(namespace, name string, leader, worker map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("leaderworkerset.x-k8s.io/v1")
	u.SetKind("LeaderWorkerSet")
	u.SetName(name)
	u.SetNamespace(namespace)
	_ = unstructured.SetNestedMap(u.Object, worker, "spec", "leaderWorkerTemplate", "workerTemplate")
	if leader != nil {
		_ = unstructured.SetNestedMap(u.Object, leader, "spec", "leaderWorkerTemplate", "leaderTemplate")
	}
	return u
}

func lwsWorkload(u *unstructured.Unstructured) Workload {
	return Workload{Kind: lwsKind, Category: CategoryInference, Namespace: u.GetNamespace(), Name: u.GetName(), Object: u}
}

func TestLWSScraper_Name_HandlesKind(t *testing.T) {
	s := newLWSScraper()
	if s.Name() != "inference.lws" {
		t.Errorf("Name() = %q", s.Name())
	}
	for k, want := range map[WorkloadKind]bool{
		lwsKind: true,
		{Group: "leaderworkerset.x-k8s.io", Version: "v1alpha1", Kind: "LeaderWorkerSet"}: false,
		{Group: "apps", Version: "v1", Kind: "StatefulSet"}:                               false,
	} {
		if got := s.HandlesKind(k); got != want {
			t.Errorf("HandlesKind(%+v) = %v, want %v", k, got, want)
		}
	}
}

func TestLWSScraper_NilObject_NilCfg(t *testing.T) {
	s := newLWSScraper()
	if _, err := s.Scrape(context.Background(), Workload{Kind: lwsKind}, testConfig()); err == nil {
		t.Error("expected error for nil Object")
	}
	if _, err := s.Scrape(context.Background(), lwsWorkload(lws("ns", "x", nil, podTemplate("nginx:1"))), nil); err == nil {
		t.Error("expected error for nil cfg")
	}
}

// Signal placement: leader-only, worker-only, both. Each case must
// attribute the runtime, place the model, and say which template
// carried it via locator and lws.role.
func TestLWSScraper_SignalPlacement(t *testing.T) {
	vllm := func() map[string]interface{} {
		return podTemplate("vllm/vllm-openai:v0.6.3", "--model", "Qwen/Qwen3-32B", "--tensor-parallel-size", "8")
	}
	plain := func() map[string]interface{} { return podTemplate("example.com/ray-worker:1") }
	cases := []struct {
		name           string
		leader, worker map[string]interface{}
		wantRoles      []string // roles that must carry the model claim
	}{
		{"leader-only", vllm(), plain(), []string{"leader"}},
		{"worker-only", nil, vllm(), []string{"worker"}},
		{"both", vllm(), vllm(), []string{"leader", "worker"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := lws("ns", "multi", tc.leader, tc.worker)
			_ = unstructured.SetNestedField(u.Object, int64(4), "spec", "leaderWorkerTemplate", "size")
			_ = unstructured.SetNestedField(u.Object, int64(2), "spec", "replicas")
			got, err := newLWSScraper().Scrape(context.Background(), lwsWorkload(u), testConfig())
			if err != nil {
				t.Fatalf("Scrape: %v", err)
			}
			models := componentsOf(got.Components, ComponentMLModel)
			if len(models) != len(tc.wantRoles) {
				t.Fatalf("models = %+v, want %d", models, len(tc.wantRoles))
			}
			gotRoles := map[string]bool{}
			for _, m := range models {
				role := m.Properties["lws.role"]
				gotRoles[role] = true
				wantLoc := "spec.leaderWorkerTemplate." + role + "Template.spec.containers[0].args[0 1](--model)"
				if m.Evidence.Locator != wantLoc {
					t.Errorf("model locator = %q, want %q", m.Evidence.Locator, wantLoc)
				}
				if m.Name != "Qwen/Qwen3-32B" || m.Confidence != ConfidenceDeclared {
					t.Errorf("model = %+v", m)
				}
			}
			for _, r := range tc.wantRoles {
				if !gotRoles[r] {
					t.Errorf("no model claim attributed to %s template; got roles %v", r, gotRoles)
				}
			}
			runtimes := componentsOf(got.Components, ComponentApplication)
			if len(runtimes) != len(tc.wantRoles) {
				t.Errorf("runtime components = %+v, want %d (one per vllm template)", runtimes, len(tc.wantRoles))
			}
			for _, r := range runtimes {
				if r.Name != "vllm" || r.Confidence != ConfidenceInferred {
					t.Errorf("runtime = %+v", r)
				}
			}
			for _, c := range componentsOf(got.Components, ComponentContainer) {
				if c.Properties["lws.size"] != "4" || c.Properties["lws.replicas"] != "2" {
					t.Errorf("container lacks group shape: %v", c.Properties)
				}
				if c.Properties["lws.role"] == "" {
					t.Errorf("container lacks lws.role: %v", c.Properties)
				}
			}
			for _, c := range got.Components {
				if strings.HasPrefix(c.Evidence.Locator, "spec.template.") {
					t.Errorf("apps/v1-shaped locator on an LWS component: %q", c.Evidence.Locator)
				}
			}
			// Declared model via args + inferred runtime via pattern → inferred overall.
			if got.Confidence != ConfidenceInferred {
				t.Errorf("workload confidence = %s, want inferred", got.Confidence)
			}
		})
	}
}

// Template annotations are read with template-rooted locators, and the
// group shape is omitted (not zeroed) when size/replicas are unset.
func TestLWSScraper_TemplateAnnotations_NoGroupShape(t *testing.T) {
	worker := podTemplate("example.com/custom:1")
	worker["metadata"] = map[string]interface{}{
		"annotations": map[string]interface{}{"model.k8saibom.dev/name": "org/annotated"},
	}
	got, err := newLWSScraper().Scrape(context.Background(), lwsWorkload(lws("ns", "ann", nil, worker)), testConfig())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	models := componentsOf(got.Components, ComponentMLModel)
	if len(models) != 1 || models[0].Evidence.Source != SourcePodTemplateAnnotation {
		t.Fatalf("models = %+v", models)
	}
	if want := "spec.leaderWorkerTemplate.workerTemplate.metadata.annotations"; !strings.HasPrefix(models[0].Evidence.Locator, want) {
		t.Errorf("annotation locator = %q, want prefix %q", models[0].Evidence.Locator, want)
	}
	for _, c := range componentsOf(got.Components, ComponentContainer) {
		for _, k := range []string{"lws.size", "lws.replicas"} {
			if _, has := c.Properties[k]; has {
				t.Errorf("%s must be absent when unset: %v", k, c.Properties)
			}
		}
	}
}

// A malformed leader template is recorded and skipped; the worker
// template still scrapes and Scrape succeeds.
func TestLWSScraper_MalformedLeader_Degrades(t *testing.T) {
	u := lws("ns", "bad", nil, podTemplate("vllm/vllm-openai:v0.6.3", "--model", "Qwen/Qwen3-0.6B"))
	_ = unstructured.SetNestedField(u.Object, "not-an-object", "spec", "leaderWorkerTemplate", "leaderTemplate")
	got, err := newLWSScraper().Scrape(context.Background(), lwsWorkload(u), testConfig())
	if err != nil {
		t.Fatalf("Scrape must succeed: %v", err)
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0].Error(), "leaderTemplate") {
		t.Errorf("errors = %v", got.Errors)
	}
	if models := componentsOf(got.Components, ComponentMLModel); len(models) != 1 || models[0].Properties["lws.role"] != "worker" {
		t.Errorf("worker model missing: %+v", models)
	}
}

// No inference signal in either template → unresolved; the reconciler
// then declines to create an AIBOM.
func TestLWSScraper_NoSignal_Unresolved(t *testing.T) {
	got, err := newLWSScraper().Scrape(context.Background(),
		lwsWorkload(lws("ns", "plain", podTemplate("example.com/a:1"), podTemplate("example.com/b:1"))), testConfig())
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if got.Confidence != ConfidenceUnresolved {
		t.Errorf("confidence = %s, want unresolved", got.Confidence)
	}
	if n := len(componentsOf(got.Components, ComponentContainer)); n != 2 {
		t.Errorf("container components = %d, want 2 (fleet visibility is kept even without AI signal)", n)
	}
}

func TestLWSScraper_Deterministic(t *testing.T) {
	u := lws("ns", "det",
		podTemplate("vllm/vllm-openai:v0.6.3", "--model", "b/model"),
		podTemplate("vllm/vllm-openai:v0.6.3", "--model", "a/model"))
	_ = unstructured.SetNestedField(u.Object, int64(2), "spec", "leaderWorkerTemplate", "size")
	s := newLWSScraper()
	a, err := s.Scrape(context.Background(), lwsWorkload(u), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Scrape(context.Background(), lwsWorkload(u.DeepCopy()), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("two scrapes differ:\n%+v\n%+v", a, b)
	}
	if len(a.Provenance) != 1 || a.Provenance[0].ScraperName != "inference.lws" {
		t.Errorf("provenance = %+v", a.Provenance)
	}
}
