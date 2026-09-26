# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.12"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# Fetch the GKE cluster credentials dynamically
data "google_client_config" "default" {}

data "google_container_cluster" "target" {
  name     = var.cluster_name
  location = var.cluster_location
  project  = var.project_id
}

provider "helm" {
  kubernetes {
    host                   = "https://${data.google_container_cluster.target.endpoint}"
    token                  = data.google_client_config.default.access_token
    cluster_ca_certificate = base64decode(data.google_container_cluster.target.master_auth.0.cluster_ca_certificate)
  }
}

# The default install deploys the PUBLISHED chart from ghcr.io: the
# image is digest-pinned by the chart, and every release carries
# provenance and SBOM attestations. Building from source is the
# explicit opt-in air-gap/development path (build_from_source = true);
# it is the only case that provisions Artifact Registry and runs
# Cloud Build.

# 1. Provision the Artifact Registry (source builds only)
resource "google_artifact_registry_repository" "aibom_repo" {
  count         = var.build_from_source ? 1 : 0
  location      = var.region
  repository_id = var.repository_id
  description   = "k8s-aibom controller image repository"
  format        = "DOCKER"
  project       = var.project_id
}

# 2. Build and push the image via Cloud Build (source builds only)
resource "null_resource" "build_image" {
  count = var.build_from_source ? 1 : 0

  lifecycle {
    precondition {
      condition     = var.image_tag != ""
      error_message = "image_tag must be set when build_from_source = true."
    }
  }

  triggers = {
    # Rebuild if the Dockerfile, Makefile, or image tag changes
    dockerfile_sha = filesha256("${path.module}/../Dockerfile")
    makefile_sha   = filesha256("${path.module}/../Makefile")
    image_tag      = var.image_tag
  }

  depends_on = [
    google_artifact_registry_repository.aibom_repo
  ]

  provisioner "local-exec" {
    command = <<EOT
      gcloud builds submit ${path.module}/../ \
        --project ${var.project_id} \
        --tag ${var.region}-docker.pkg.dev/${var.project_id}/${var.repository_id}/k8s-aibom:${var.image_tag}
    EOT
  }
}

# 3. Deploy the Helm chart. Default: the published OCI chart at a
# pinned version, image untouched (digest-pinned by the chart).
# Source builds: the local chart with the image overridden.
resource "helm_release" "k8s_aibom" {
  name             = "k8s-aibom"
  chart            = var.build_from_source ? "${path.module}/../charts/k8s-aibom" : "oci://ghcr.io/googlecloudplatform/charts/k8s-aibom"
  version          = var.build_from_source ? null : var.chart_version
  namespace        = var.namespace
  create_namespace = true

  dynamic "set" {
    for_each = var.build_from_source ? [1] : []
    content {
      name  = "image.repository"
      value = "${var.region}-docker.pkg.dev/${var.project_id}/${var.repository_id}/k8s-aibom"
    }
  }

  dynamic "set" {
    for_each = var.build_from_source ? [1] : []
    content {
      name  = "image.tag"
      value = var.image_tag
    }
  }

  # Ensure the image is built and pushed before Helm tries to pull it
  # (empty when build_from_source = false).
  depends_on = [null_resource.build_image]
}
