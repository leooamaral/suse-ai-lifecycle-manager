/*
Copyright 2025.

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
	"fmt"
	"time"

	urlpkg "net/url"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/cli"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiplatformv1beta1 "github.com/SUSE/suse-ai-operator/api/v1beta1"
	helmClient "github.com/SUSE/suse-ai-operator/internal/infra/helm"
	"github.com/SUSE/suse-ai-operator/internal/infra/kubernetes"
	"github.com/SUSE/suse-ai-operator/internal/infra/rancher"
	"github.com/SUSE/suse-ai-operator/internal/installaiextension"
)

const (
	readinessTimeout = 5 * time.Minute

	annotationLastSourceType  = "ai-platform.suse.com/last-source-type"
	annotationLastHelmRelease = "ai-platform.suse.com/last-helm-release-name"
	annotationLastClusterRepo = "ai-platform.suse.com/last-cluster-repo-name"
)

// InstallAIExtensionReconciler reconciles a InstallAIExtension object
type InstallAIExtensionReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	Recorder           record.EventRecorder
	Config             *rest.Config
	ExtensionNamespace string
}

// +kubebuilder:rbac:groups=ai-platform.suse.com,resources=installaiextensions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai-platform.suse.com,resources=installaiextensions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ai-platform.suse.com,resources=installaiextensions/finalizers,verbs=update
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list

// +kubebuilder:rbac:groups=catalog.cattle.io,resources=clusterrepos,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=catalog.cattle.io,resources=clusterrepos/status,verbs=get;update;patch

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *InstallAIExtensionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	namespace := r.ExtensionNamespace

	var installExt aiplatformv1beta1.InstallAIExtension
	if err := r.Get(ctx, req.NamespacedName, &installExt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rancherMgr := rancher.NewManager(r.Client, r.Scheme)

	// Handle deletion first (for both source types)
	if !installExt.ObjectMeta.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, &installExt, rancherMgr, namespace); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	added, err := r.ensureFinalizer(ctx, &installExt)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		return ctrl.Result{Requeue: true}, nil
	}

	// Check extension name uniqueness
	if err := r.checkExtensionNameUniqueness(ctx, &installExt); err != nil {
		if setErr := r.setStatus(ctx, &installExt, aiplatformv1beta1.PhaseFailed, err.Error()); setErr != nil {
			log.Error(setErr, "failed to update status")
		}
		return ctrl.Result{}, nil // user error, don't requeue
	}

	// Detect source type change and clean up old resources
	if err := r.detectAndCleanupSourceChange(ctx, log, &installExt, rancherMgr, namespace); err != nil {
		if setErr := r.setStatus(ctx, &installExt, aiplatformv1beta1.PhaseFailed, err.Error()); setErr != nil {
			log.Error(setErr, "failed to update status")
		}
		return ctrl.Result{}, err
	}

	var svcURL string

	switch {
	case installExt.Spec.Source.Helm != nil:
		svcURL, err = r.reconcileHelmSource(ctx, log, &installExt, namespace)
		if err != nil {
			if setErr := r.setStatus(ctx, &installExt, aiplatformv1beta1.PhaseFailed, err.Error()); setErr != nil {
				log.Error(setErr, "failed to update status")
			}
			return ctrl.Result{}, err
		}

		// Check if deployment is actually ready
		releaseName := installExt.Spec.Source.Helm.Name
		ready, readyErr := kubernetes.IsDeploymentReady(ctx, r.Client, namespace, releaseName, log)
		if readyErr != nil {
			log.Error(readyErr, "Failed to check deployment readiness", "release", releaseName, "namespace", namespace)
			return r.handleNotReady(ctx, &installExt, log)
		}
		if !ready {
			log.Info("Deployment is not ready yet", "release", releaseName, "namespace", namespace)
			return r.handleNotReady(ctx, &installExt, log)
		}

	case installExt.Spec.Source.Git != nil:
		// Git: no deployment to check

	default:
		return ctrl.Result{}, fmt.Errorf("source must specify either helm or git")
	}

	// Ensure Rancher resources
	if err := rancherMgr.Ensure(ctx, &installExt, svcURL, namespace); err != nil {
		if setErr := r.setStatus(ctx, &installExt, aiplatformv1beta1.PhaseFailed, err.Error()); setErr != nil {
			log.Error(setErr, "failed to update status")
		}
		return ctrl.Result{}, err
	}

	// Track current source type in annotations for future change detection
	if err := r.updateSourceAnnotations(ctx, &installExt); err != nil {
		return ctrl.Result{}, err
	}

	// Everything succeeded — only update status if not already installed
	if installExt.Status.Phase != aiplatformv1beta1.PhaseInstalled {
		msg := fmt.Sprintf("Extension %s installed", installExt.Spec.Extension.Name)
		if err := r.setStatus(ctx, &installExt, aiplatformv1beta1.PhaseInstalled, msg); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *InstallAIExtensionReconciler) setStatus(
	ctx context.Context,
	ext *aiplatformv1beta1.InstallAIExtension,
	phase, message string,
) error {
	ext.Status.Phase = phase
	ext.Status.Message = message
	return r.Status().Update(ctx, ext)
}

func (r *InstallAIExtensionReconciler) handleNotReady(
	ctx context.Context,
	ext *aiplatformv1beta1.InstallAIExtension,
	log logr.Logger,
) (ctrl.Result, error) {

	// Only set installing if not already in that state
	if ext.Status.Phase != aiplatformv1beta1.PhaseInstalling {
		if err := r.setStatus(ctx, ext, aiplatformv1beta1.PhaseInstalling, "Waiting for deployment to be ready"); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Use annotation to track when we started waiting, since we no longer have conditions
	const waitingSinceAnnotation = "ai-platform.suse.com/waiting-since"
	var waitingSince time.Time

	if ext.Annotations != nil {
		if ts, ok := ext.Annotations[waitingSinceAnnotation]; ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				waitingSince = t
			}
		}
	}

	if waitingSince.IsZero() {
		waitingSince = time.Now()
		if ext.Annotations == nil {
			ext.Annotations = make(map[string]string)
		}
		ext.Annotations[waitingSinceAnnotation] = waitingSince.Format(time.RFC3339)
		if err := r.Update(ctx, ext); err != nil {
			return ctrl.Result{}, err
		}
	}

	if time.Since(waitingSince) > readinessTimeout {
		msg := fmt.Sprintf("Deployment not ready after %s", readinessTimeout)
		if err := r.setStatus(ctx, ext, aiplatformv1beta1.PhaseFailed, msg); err != nil {
			return ctrl.Result{}, err
		}
		// Clean up annotation
		delete(ext.Annotations, waitingSinceAnnotation)
		if len(ext.Annotations) == 0 {
			ext.Annotations = nil
		}
		if err := r.Update(ctx, ext); err != nil {
			return ctrl.Result{}, err
		}
		// Still requeue — deployment might recover
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	elapsed := time.Since(waitingSince).Truncate(time.Second)
	log.Info("Waiting for deployment to be ready", "elapsed", elapsed)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// --- Extension name uniqueness ---

func (r *InstallAIExtensionReconciler) checkExtensionNameUniqueness(
	ctx context.Context,
	ext *aiplatformv1beta1.InstallAIExtension,
) error {
	var list aiplatformv1beta1.InstallAIExtensionList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("failed to list InstallAIExtension resources: %w", err)
	}

	for _, other := range list.Items {
		if other.Name == ext.Name {
			continue
		}
		if other.Spec.Extension.Name == ext.Spec.Extension.Name {
			return fmt.Errorf(
				"extension name %q is already used by InstallAIExtension %q",
				ext.Spec.Extension.Name,
				other.Name,
			)
		}
	}
	return nil
}

// --- Source change detection and cleanup ---

func currentSourceType(ext *aiplatformv1beta1.InstallAIExtension) string {
	if ext.Spec.Source.Helm != nil {
		return "helm"
	}
	if ext.Spec.Source.Git != nil {
		return "git"
	}
	return ""
}

func (r *InstallAIExtensionReconciler) detectAndCleanupSourceChange(
	ctx context.Context,
	log logr.Logger,
	ext *aiplatformv1beta1.InstallAIExtension,
	rancherMgr *rancher.Manager,
	namespace string,
) error {
	if ext.Annotations == nil {
		return nil // first reconcile, nothing to clean up
	}

	lastSource := ext.Annotations[annotationLastSourceType]
	if lastSource == "" {
		return nil // no previous source tracked
	}

	current := currentSourceType(ext)
	if lastSource == current {
		return nil // no change
	}

	log.Info("Source type changed, cleaning up old resources",
		"previousSource", lastSource, "currentSource", current)

	// Clean up old helm release if switching away from helm
	if lastSource == "helm" {
		oldRelease := ext.Annotations[annotationLastHelmRelease]
		if oldRelease != "" {
			log.Info("Deleting old helm release", "release", oldRelease)
			settings := cli.New()
			settings.SetNamespace(namespace)
			helm, err := helmClient.New(settings)
			if err != nil {
				return fmt.Errorf("failed to create helm client for cleanup: %w", err)
			}
			if err := helm.DeleteRelease(ctx, oldRelease); err != nil {
				return fmt.Errorf("failed to delete old helm release %q: %w", oldRelease, err)
			}
		}
	}

	// Clean up old ClusterRepo if the name changed
	oldClusterRepo := ext.Annotations[annotationLastClusterRepo]
	if oldClusterRepo != "" {
		newClusterRepo := ""
		if ext.Spec.Source.Helm != nil {
			newClusterRepo = ext.Spec.Source.Helm.Name
		} else {
			newClusterRepo = ext.Spec.Extension.Name
		}
		if oldClusterRepo != newClusterRepo {
			log.Info("Deleting old ClusterRepo", "oldName", oldClusterRepo, "newName", newClusterRepo)
			if err := rancherMgr.DeleteClusterRepoByName(ctx, oldClusterRepo); err != nil {
				return fmt.Errorf("failed to delete old ClusterRepo %q: %w", oldClusterRepo, err)
			}
		}
	}

	return nil
}

func (r *InstallAIExtensionReconciler) updateSourceAnnotations(
	ctx context.Context,
	ext *aiplatformv1beta1.InstallAIExtension,
) error {
	if ext.Annotations == nil {
		ext.Annotations = make(map[string]string)
	}

	current := currentSourceType(ext)
	ext.Annotations[annotationLastSourceType] = current

	switch {
	case ext.Spec.Source.Helm != nil:
		ext.Annotations[annotationLastHelmRelease] = ext.Spec.Source.Helm.Name
		ext.Annotations[annotationLastClusterRepo] = ext.Spec.Source.Helm.Name
	case ext.Spec.Source.Git != nil:
		delete(ext.Annotations, annotationLastHelmRelease)
		ext.Annotations[annotationLastClusterRepo] = ext.Spec.Extension.Name
	}

	return r.Update(ctx, ext)
}

// --- Helm source reconciliation ---

func (r *InstallAIExtensionReconciler) reconcileHelmSource(
	ctx context.Context,
	log logr.Logger,
	installExt *aiplatformv1beta1.InstallAIExtension,
	namespace string,
) (string, error) {
	releaseName := installExt.Spec.Source.Helm.Name
	chartVersion := installExt.Spec.Source.Helm.Version
	values, err := helmClient.ConvertHelmValues(installExt.Spec.Source.Helm.Values)
	if err != nil {
		log.Error(err, "failed to convert Helm values")
		return "", err
	}
	url := installExt.Spec.Source.Helm.URL
	u, err := urlpkg.Parse(url)
	if err != nil {
		log.Error(err, "invalid helm url", "url", url)
		return "", err
	}

	var chart string
	switch u.Scheme {
	case "oci", "https":
		chart = url
	default:
		return "", fmt.Errorf("unsupported helm url scheme: %s", u.Scheme)
	}

	settings := cli.New()
	settings.SetNamespace(namespace)

	helm, err := helmClient.New(settings)
	if err != nil {
		log.Error(err, "failed to create Helm client")
		return "", err
	}

	err = helm.EnsureRelease(ctx, helmClient.ReleaseSpec{
		Name:      releaseName,
		Namespace: namespace,
		ChartRef:  chart,
		Version:   chartVersion,
		Values:    values,
	})
	if err != nil {
		return "", err
	}

	svc, err := kubernetes.ServiceForHelmRelease(ctx, r.Client, namespace, releaseName)
	if err != nil {
		log.Info("Error to fetch services")
		return "", err
	}

	svcName, svcNamespace, svcPort, err := installaiextension.ServiceEndpoint(svc)
	if err != nil {
		log.Info("Error to fetch svc info")
		return "", err
	}

	return fmt.Sprintf("http://%s.%s:%d", svcName, svcNamespace, svcPort), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstallAIExtensionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiplatformv1beta1.InstallAIExtension{}).
		Named("InstallAIExtension").
		Complete(r)
}
