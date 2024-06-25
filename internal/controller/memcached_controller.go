/*
Copyright 2024.

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
	"net/http"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	log "sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/mmadert/memcached-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"                   // added
	corev1 "k8s.io/api/core/v1"                   // added
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // added
	"k8s.io/apimachinery/pkg/types"               // added

	goversion "github.com/hashicorp/go-version"
)

// MemcachedReconciler reconciles a Memcached object
type MemcachedReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=cache.example.com,resources=memcacheds,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.example.com,resources=memcacheds/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.example.com,resources=memcacheds/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete // added
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list; // added

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Memcached object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *MemcachedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// 1. Fetch the memcached instance
	memcached := &cachev1alpha1.Memcached{}
	err := r.Get(ctx, req.NamespacedName, memcached)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("1. Fetch the memcached instance. Memcached resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}

		// Error reading the object - requeue request
		return ctrl.Result{}, err
	}

	log.Info("1. Fetch the memcached instance. Memcached resource found", "memcached.Name", memcached.Name, "memcached.Namespace", memcached.Namespace)

	// 2. Check if deployment exists, if not create one.
	specVersion := memcached.Spec.Version
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, deployment)
	if err != nil && errors.IsNotFound(err) {
		// Define new deployment
		dep := r.deploymentForMemcached(memcached, specVersion)
		log.Info("2. Check if deployment exists, if not create one. Create a new deployment", "Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		err = r.Create(ctx, dep)
		if err != nil {
			log.Error(err, "2. Check if deployment exists, if not create one. Create a new deployment. Failed to create new deployment", "Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// Deployment created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "2. Check if deployment exists, if not create one. Failed to get deployment.")
		return ctrl.Result{}, err
	}

	// 3. Ensure the deployment is the same size as the spec
	size := memcached.Spec.Size
	if *deployment.Spec.Replicas != size {
		deployment.Spec.Replicas = &size // use same ref is safe?
		err := r.Update(ctx, deployment)
		if err != nil {
			log.Error(err, "3. Ensure the deployment is the same size as the spec. Failed to update Deployment", "Deployment.Namespace", deployment.Namespace, "Deployment.Name", deployment.Name)
			return ctrl.Result{}, err
		}

		log.Info("3. Ensure the deployment is the same size as the spec. Update deployment size", "Deployment.Spec.Replicas", size)
		return ctrl.Result{Requeue: true}, nil
	}

	// 4. Store pod names in status
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(memcached.Namespace),
		client.MatchingLabels(labelsForMemcached(memcached.Name)),
	}

	err = r.List(ctx, podList, listOpts...)
	if err != nil {
		log.Error(err, "4. Update the Memcached status with the pod names. Failed to list pods", "Memcached.Namespace", memcached.Namespace, "Memcached.Name", memcached.Name)
		return ctrl.Result{}, err
	}
	podNames := getPodNames(podList.Items)
	log.Info("4. Update the Memcached status with the pod names. Pod list", "podNames", podNames)

	// Update status.Nodes if needed
	if !reflect.DeepEqual(podNames, memcached.Status.Nodes) {
		memcached.Status.Nodes = podNames
		err := r.Status().Update(ctx, memcached)
		if err != nil {
			log.Error(err, "4. Update the Memcached status with the pod names. Failed to update status")
			return ctrl.Result{}, err
		}
	}

	log.Info("4. Update the Memcached status with the pod names. Update memcached.Status", "memcached.Status.Nodes", memcached.Status.Nodes)

	// 5. Ensure the deployment image matches the desired version
	container := &deployment.Spec.Template.Spec.Containers[0]
	currentVersion := versionFromImage(container.Image)

	if currentVersion != specVersion {
		valid, err := isValidVersion(specVersion)
		if err != nil {
			log.Error(err, "5. Update the memcached version. Unexpected error checking version")
			return ctrl.Result{}, err
		}

		if !valid {
			log.Error(err, "5. Update the memcached version. New version is invalid.")
			return ctrl.Result{}, nil
		}

		minorUpgrade, err := isMinorUpgrade(currentVersion, specVersion)
		if err != nil {
			log.Error(err, "5. Update the memcached version. Unexpected error comparing versions.")
			return ctrl.Result{}, err
		}

		if !minorUpgrade {
			log.Error(err, "5. Update the memcached version. New version is not a minor upgrade.")
			return ctrl.Result{}, nil
		}

		image := imageName(specVersion)
		container.Image = image
		err = r.Update(ctx, deployment)
		if err != nil {
			log.Info("5. Ensure the deployment image matches the current valid version. Update deployment. Failed to update deployment", "Deployment.Namespace", deployment.Namespace, "Deployment.Name", deployment.Name)
			return ctrl.Result{}, err
		}

		log.Info("5. Ensure the deployment image matches the current valid version. Update deployment", "container.Image", image)
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MemcachedReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Memcached{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func (r *MemcachedReconciler) deploymentForMemcached(m *cachev1alpha1.Memcached, version string) *appsv1.Deployment {
	ls := labelsForMemcached(m.Name)
	replicas := m.Spec.Size

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Image: imageName(version),
							Name:  "memcached",
							// Command: []string{"memcached", "-m=64", "-o", "modern", "-v"},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 11211,
									Name:          "memcached",
								},
							},
						},
					},
				},
			},
		},
	}

	// Memcached instance controls the deployment
	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep
}

func labelsForMemcached(name string) map[string]string {
	return map[string]string{"app": "memcached", "memcached_cr": name}
}

func getPodNames(pods []corev1.Pod) []string {
	var names []string

	for _, pod := range pods {
		names = append(names, pod.Name)
	}

	return names
}

// isValidVersion queries the docker hub API to check if the provided version can be resolved to an image tag.
func isValidVersion(version string) (bool, error) {
	// Check if version is valid Semver
	_, err := goversion.NewSemver(version)
	if err != nil {
		// Assume error means the version is malformed.
		return false, nil
	}

	// Check if tag exists in docker hub
	tag := imageTag(version)
	url := fmt.Sprintf("http://hub.docker.com/v2/repositories/library/memcached/tags/%s", tag)
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("Error checking tag:", err)
		return false, err
	}
	defer resp.Body.Close()

	fmt.Println("Got status:", resp.StatusCode)

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	return false, fmt.Errorf("unexpected statuscode while checking dockerhub tag: %d", resp.StatusCode)
}

// isMinorUpgrade checks if the new (target) version represents a minor version upgrade from the current one.
func isMinorUpgrade(currentVersion string, targetVersion string) (bool, error) {
	current, err := goversion.NewSemver(currentVersion)
	if err != nil {
		return false, err
	}

	target, err := goversion.NewSemver(targetVersion)
	if err != nil {
		return false, err
	}

	if current.GreaterThanOrEqual(target) {
		// Downgrade or equal.
		return false, nil
	}

	if current.Core().Segments()[0] != target.Core().Segments()[0] {
		// target is a major upgrade
		return false, nil
	}

	return true, nil
}

func imageTag(version string) string {
	return version + "-alpine"
}

func imageName(version string) string {
	return "memcached:" + imageTag(version)
}

func versionFromImage(image string) string {
	return strings.Split(strings.TrimSuffix(image, "-alpine"), ":")[1]
}
