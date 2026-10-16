# k8s-aibom-crds

The CustomResourceDefinitions for [k8s-aibom](https://github.com/GoogleCloudPlatform/k8s-aibom),
`aiboms.aibom.k8saibom.dev` and `aibomcontrollerconfigs.aibom.k8saibom.dev`,
packaged as ordinary chart templates so that `helm upgrade` applies schema
changes. Helm never upgrades CRDs shipped in a chart's `crds/` directory,
which is how the main `k8s-aibom` chart carries them; a controller upgrade
whose CRDs changed would otherwise run against the previous schema, with
the API server silently pruning fields the old schema does not know.

Published by the same tag-triggered pipeline as the main chart, with the
same version, and attested like every other release artifact. The main
chart keeps its `crds/` directory, so single-chart installs keep working;
Helm skips `crds/` entries that already exist, so the two charts do not
conflict.

## Install

```bash
helm install k8s-aibom-crds oci://ghcr.io/googlecloudplatform/charts/k8s-aibom-crds \
  --version <version> --namespace k8s-aibom-system --create-namespace
helm install k8s-aibom oci://ghcr.io/googlecloudplatform/charts/k8s-aibom \
  --version <version> --namespace k8s-aibom-system
```

## Upgrade

Upgrade this chart first, then the main chart, both to the same version:

```bash
helm upgrade k8s-aibom-crds oci://ghcr.io/googlecloudplatform/charts/k8s-aibom-crds --version <new> -n k8s-aibom-system
helm upgrade k8s-aibom      oci://ghcr.io/googlecloudplatform/charts/k8s-aibom      --version <new> -n k8s-aibom-system
```

## Adopting on an existing install

CRDs installed earlier by the main chart's `crds/` directory (or by
`install.yaml`) are not owned by a Helm release. Adopt them into this
chart's release once, then upgrade normally:

```bash
for crd in aiboms.aibom.k8saibom.dev aibomcontrollerconfigs.aibom.k8saibom.dev; do
  kubectl annotate crd "$crd" meta.helm.sh/release-name=k8s-aibom-crds meta.helm.sh/release-namespace=k8s-aibom-system --overwrite
  kubectl label    crd "$crd" app.kubernetes.io/managed-by=Helm --overwrite
done
helm install k8s-aibom-crds oci://ghcr.io/googlecloudplatform/charts/k8s-aibom-crds --version <current> -n k8s-aibom-system
```

## Uninstall

`helm uninstall k8s-aibom-crds` leaves the CRDs in place
(`helm.sh/resource-policy: keep`): deleting them deletes every AIBOM in
the cluster. To purge deliberately:

```bash
kubectl delete crd aiboms.aibom.k8saibom.dev aibomcontrollerconfigs.aibom.k8saibom.dev
```
