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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	aibomv1beta1 "github.com/GoogleCloudPlatform/k8s-aibom/api/v1beta1"
	"github.com/GoogleCloudPlatform/k8s-aibom/internal/bom"
	"github.com/GoogleCloudPlatform/k8s-aibom/internal/scraper"
)

// dynamoGraphDeploymentGVK is the GroupVersionKind this reconciler
// watches. Pinned to v1beta1 (the operator's storage version) per
// docs/external-crd-versions.md.
var dynamoGraphDeploymentGVK = schema.GroupVersionKind{
	Group:   "nvidia.com",
	Version: "v1beta1",
	Kind:    "DynamoGraphDeployment",
}

// DynamoGraphDeploymentReconciler watches NVIDIA Dynamo
// DynamoGraphDeployment CRs in opted-in namespaces and produces one
// AIBOM per graph via DynamoGraphDeploymentScraper (Design 003 §4).
//
// Like the KServe reconciler it fetches and watches
// *unstructured.Unstructured with an explicit GVK (no operator Go
// module dependency) and lists no pods: the operator materializes
// DynamoComponentDeployments → Deployments / LeaderWorkerSets / Grove
// resources → Pods, and following that chain is the Design 003 §3
// ownership roll-up, which lands separately. Workload.Pods is an empty
// (non-nil) slice so downstream code sees a stable zero-length result.
type DynamoGraphDeploymentReconciler struct {
	WorkloadReconciler
}

// +kubebuilder:rbac:groups=nvidia.com,resources=dynamographdeployments,verbs=get;list;watch

func (r *DynamoGraphDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("dynamographdeployment", req.NamespacedName))

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(dynamoGraphDeploymentGVK)
	if err := r.Get(ctx, req.NamespacedName, u); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	workload := scraper.Workload{
		Kind:      scraper.WorkloadKind{Group: "nvidia.com", Version: "v1beta1", Kind: "DynamoGraphDeployment"},
		Category:  scraper.CategoryInference,
		Namespace: u.GetNamespace(),
		Name:      u.GetName(),
		UID:       u.GetUID(),
		Object:    u,
		Pods:      []corev1.Pod{},
	}
	return r.reconcileWorkload(ctx, WorkloadReconcileRequest{
		Workload:  workload,
		AIBOMName: AIBOMNameForWorkload("nvidia.com", "DynamoGraphDeployment", u.GetName()),
		SetOwnerReference: func(a *aibomv1beta1.AIBOM) error {
			return controllerutil.SetControllerReference(u, a, r.Scheme)
		},
		BOMBuildOptions: bom.BuildOptions{
			WorkloadKind:      "DynamoGraphDeployment",
			WorkloadGroup:     "nvidia.com",
			WorkloadAPIVer:    "v1beta1",
			WorkloadNamespace: u.GetNamespace(),
			WorkloadName:      u.GetName(),
			WorkloadUID:       string(u.GetUID()),
			WorkloadCategory:  string(scraper.CategoryInference),
			ControllerName:    r.ControllerName,
			ControllerVersion: r.ControllerVersion,
		},
		SummaryOptions: SummaryOptions{
			WorkloadKind:       "DynamoGraphDeployment",
			WorkloadAPIVersion: "nvidia.com/v1beta1",
			WorkloadName:       u.GetName(),
			WorkloadNamespace:  u.GetNamespace(),
			WorkloadCategory:   string(scraper.CategoryInference),
		},
		Generation: u.GetGeneration(),
	})
}

// SetupWithManager registers this reconciler with the controller-runtime
// manager. The watch is on *unstructured.Unstructured with the pinned
// GVK; no scheme registration of the Dynamo types is required.
func (r *DynamoGraphDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(dynamoGraphDeploymentGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		Owns(&aibomv1beta1.AIBOM{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.EnqueueWorkloadsForNamespace(
				func() client.ObjectList {
					list := &unstructured.UnstructuredList{}
					list.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   "nvidia.com",
						Version: "v1beta1",
						Kind:    "DynamoGraphDeploymentList",
					})
					return list
				},
				func(objList client.ObjectList) []client.Object {
					uList, ok := objList.(*unstructured.UnstructuredList)
					if !ok {
						return nil
					}
					var objs []client.Object
					for i := range uList.Items {
						objs = append(objs, &uList.Items[i])
					}
					return objs
				},
			)),
			builder.WithPredicates(r.NamespaceWatchPredicate()),
		).
		Complete(r)
}
