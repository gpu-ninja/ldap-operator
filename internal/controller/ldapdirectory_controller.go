/* SPDX-License-Identifier: Apache-2.0
 *
 * Copyright 2023 Damian Peckett <damian@pecke.tt>.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ldapv1alpha1 "github.com/gpu-ninja/ldap-operator/api/v1alpha1"
	"github.com/gpu-ninja/operator-utils/password"
	"github.com/gpu-ninja/operator-utils/updater"
	"github.com/gpu-ninja/operator-utils/zaplogr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Allow recording of events.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch

// Need to be able to read secrets to get the TLS certificates / passwords, etc.
// Need to be able to create secrets to store the generated admin password.
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Need to be able to manage statefulsets and services.
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

//+kubebuilder:rbac:groups=ldap.gpu-ninja.com,resources=ldapdirectories,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ldap.gpu-ninja.com,resources=ldapdirectories/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ldap.gpu-ninja.com,resources=ldapdirectories/finalizers,verbs=update

const (
	// FinalizerName is the name of the finalizer used by controllers.
	FinalizerName = "ldap.gpu-ninja.com/finalizer"
	// adminPasswordLength is the length of the randomly generated admin password.
	adminPasswordLength = 32
	// reconcileRetryInterval is the interval at which the controller will retry
	// to reconcile a resource.
	reconcileRetryInterval = 5 * time.Second
)

type LDAPDirectoryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *LDAPDirectoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := zaplogr.FromContext(ctx)

	var directory ldapv1alpha1.LDAPDirectory
	if err := r.Get(ctx, req.NamespacedName, &directory); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(&directory, FinalizerName) {
		logger.Info("Adding Finalizer")

		_, err := controllerutil.CreateOrPatch(ctx, r.Client, &directory, func() error {
			controllerutil.AddFinalizer(&directory, FinalizerName)

			return nil
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	if !directory.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Deleting")

		// Nothing to do here, as the downstream resources will be garbage collected.
		// Perhaps a reclaim policy to clean up the pvcs could be added in the future.

		if controllerutil.ContainsFinalizer(&directory, FinalizerName) {
			logger.Info("Removing Finalizer")

			_, err := controllerutil.CreateOrPatch(ctx, r.Client, &directory, func() error {
				controllerutil.RemoveFinalizer(&directory, FinalizerName)

				return nil
			})
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
		}

		return ctrl.Result{}, nil
	}

	ok, err := directory.ResolveReferences(ctx, r.Client, r.Scheme)
	if !ok && err == nil {
		logger.Info("Not all references are resolvable, requeuing")

		r.Recorder.Event(&directory, corev1.EventTypeWarning,
			"NotReady", "Not all references are resolvable")

		if err := r.markPending(ctx, &directory); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: reconcileRetryInterval}, nil
	} else if err != nil {
		r.Recorder.Eventf(&directory, corev1.EventTypeWarning,
			"Failed", "Failed to resolve references: %s", err)

		r.markFailed(ctx, &directory,
			fmt.Errorf("failed to resolve references: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to resolve references: %w", err)
	}

	logger.Info("Creating or updating")

	logger.Info("Creating or updating admin password secret")

	adminPasswordSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("ldap-%s-admin-password", directory.Name),
			Namespace: directory.Namespace,
		},
	}

	// Get the admin password.
	if err = r.Get(ctx, client.ObjectKeyFromObject(&adminPasswordSecret), &adminPasswordSecret); err != nil {
		if apierrors.IsNotFound(err) {
			pw, err := password.Generate(adminPasswordLength)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to generate random admin password: %w", err)
			}

			adminPasswordSecret.Data = map[string][]byte{
				"password": []byte(pw),
			}

			if err := controllerutil.SetControllerReference(&directory, &adminPasswordSecret, r.Scheme); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to set owner reference on admin password secret: %w", err)
			}

			if err := r.Create(ctx, &adminPasswordSecret); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create admin password secret: %w", err)
			}
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get admin password secret: %w", err)
		}
	}

	logger.Info("Reconciling statefulset")

	sts, err := r.statefulSetTemplate(&directory)
	if err != nil {
		r.Recorder.Eventf(&directory, corev1.EventTypeWarning,
			"Failed", "Failed to generate statefulset template: %s", err)

		r.markFailed(ctx, &directory,
			fmt.Errorf("failed to generate statefulset template: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to generate statefulset template: %w", err)
	}

	if _, err := updater.CreateOrUpdateFromTemplate(ctx, r.Client, sts); err != nil {
		r.Recorder.Eventf(&directory, corev1.EventTypeWarning,
			"Failed", "Failed to reconcile statefulset: %s", err)

		r.markFailed(ctx, &directory,
			fmt.Errorf("failed to reconcile statefulset: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to reconcile statefulset: %w", err)
	}

	logger.Info("Reconciling service")

	svc, err := r.serviceTemplate(&directory)
	if err != nil {
		r.Recorder.Eventf(&directory, corev1.EventTypeWarning,
			"Failed", "Failed to generate service template: %s", err)

		r.markFailed(ctx, &directory,
			fmt.Errorf("failed to generate service template: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to generate service template: %w", err)
	}

	if _, err := updater.CreateOrUpdateFromTemplate(ctx, r.Client, svc); err != nil {
		r.Recorder.Eventf(&directory, corev1.EventTypeWarning,
			"Failed", "Failed to reconcile service: %s", err)

		r.markFailed(ctx, &directory,
			fmt.Errorf("failed to reconcile service: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to reconcile service: %w", err)
	}

	ready, err := r.isStatefulSetReady(ctx, &directory)
	if err != nil {
		r.Recorder.Eventf(&directory, corev1.EventTypeWarning,
			"Failed", "Failed to check if statefulset is ready: %s", err)

		r.markFailed(ctx, &directory,
			fmt.Errorf("failed to check if statefulset is ready: %w", err))

		return ctrl.Result{}, fmt.Errorf("failed to check if statefulset is ready: %w", err)
	}

	if !ready {
		logger.Info("Waiting for statefulset to become ready")

		r.Recorder.Event(&directory, corev1.EventTypeNormal,
			"Pending", "Waiting for statefulset to become ready")

		if err := r.markPending(ctx, &directory); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: reconcileRetryInterval}, nil
	}

	if directory.Status.Phase != ldapv1alpha1.LDAPDirectoryPhaseReady {
		r.Recorder.Event(&directory, corev1.EventTypeNormal,
			"Created", "Successfully created")

		if err := r.markReady(ctx, &directory); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *LDAPDirectoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.LDAPDirectory{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func (r *LDAPDirectoryReconciler) markPending(ctx context.Context, directory *ldapv1alpha1.LDAPDirectory) error {
	key := client.ObjectKeyFromObject(directory)
	err := updater.UpdateStatus(ctx, r.Client, key, directory, func() error {
		directory.Status.ObservedGeneration = directory.ObjectMeta.Generation
		directory.Status.Phase = ldapv1alpha1.LDAPDirectoryPhasePending

		meta.SetStatusCondition(&directory.Status.Conditions, metav1.Condition{
			Type:               string(ldapv1alpha1.LDAPDirectoryConditionTypePending),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: directory.ObjectMeta.Generation,
			Reason:             "Pending",
			Message:            "LDAP directory is pending",
		})

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to mark as pending: %w", err)
	}

	return nil
}

func (r *LDAPDirectoryReconciler) markReady(ctx context.Context, directory *ldapv1alpha1.LDAPDirectory) error {
	key := client.ObjectKeyFromObject(directory)
	err := updater.UpdateStatus(ctx, r.Client, key, directory, func() error {
		directory.Status.ObservedGeneration = directory.ObjectMeta.Generation
		directory.Status.Phase = ldapv1alpha1.LDAPDirectoryPhaseReady

		meta.SetStatusCondition(&directory.Status.Conditions, metav1.Condition{
			Type:               string(ldapv1alpha1.LDAPDirectoryConditionTypeReady),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: directory.ObjectMeta.Generation,
			Reason:             "Ready",
			Message:            "LDAP directory is ready",
		})

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to mark as ready: %w", err)
	}

	return nil
}

func (r *LDAPDirectoryReconciler) markFailed(ctx context.Context, directory *ldapv1alpha1.LDAPDirectory, err error) {
	logger := zaplogr.FromContext(ctx)

	key := client.ObjectKeyFromObject(directory)
	updateErr := updater.UpdateStatus(ctx, r.Client, key, directory, func() error {
		directory.Status.ObservedGeneration = directory.ObjectMeta.Generation
		directory.Status.Phase = ldapv1alpha1.LDAPDirectoryPhaseFailed

		meta.SetStatusCondition(&directory.Status.Conditions, metav1.Condition{
			Type:               string(ldapv1alpha1.LDAPDirectoryConditionTypeFailed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: directory.ObjectMeta.Generation,
			Reason:             "Failed",
			Message:            err.Error(),
		})

		return nil
	})
	if updateErr != nil {
		logger.Error("Failed to mark as failed", zap.Error(updateErr))
	}
}

func (r *LDAPDirectoryReconciler) statefulSetTemplate(directory *ldapv1alpha1.LDAPDirectory) (*appsv1.StatefulSet, error) {
	envVars := []corev1.EnvVar{
		{
			Name:  "LDAP_DOMAIN",
			Value: directory.Spec.Domain,
		},
		{
			Name:  "LDAP_ORGANIZATION",
			Value: directory.Spec.Organization,
		},
		{
			Name: "LDAP_ADMIN_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: fmt.Sprintf("ldap-%s-admin-password", directory.Name),
					},
					Key: "password",
				},
			},
		},
	}

	if directory.Spec.FileDescriptorLimit != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LDAP_NOFILE",
			Value: strconv.Itoa(*directory.Spec.FileDescriptorLimit),
		})
	}
	if directory.Spec.DebugLevel != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LDAP_DEBUG_LEVEL",
			Value: strconv.Itoa(*directory.Spec.DebugLevel),
		})
	}

	volumeClaimTemplates := defaultVolumeClaimTemplates()

	for _, volumeClaimTemplate := range directory.Spec.VolumeClaimTemplates {
		var found bool
		for i, existingVolumeClaimTemplate := range volumeClaimTemplates {
			if existingVolumeClaimTemplate.Name == volumeClaimTemplate.Name {
				volumeClaimTemplates[i] = volumeClaimTemplate
				found = true
				break
			}
		}

		if !found {
			volumeClaimTemplates = append(volumeClaimTemplates, volumeClaimTemplate)
		}
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "config",
			MountPath: "/etc/ldap/slapd.d",
		},
		{
			Name:      "data",
			MountPath: "/var/lib/ldap",
		},
		{
			Name:      "certs",
			MountPath: "/etc/ldap/certs",
		},
	}

	for _, volumeMount := range directory.Spec.VolumeMounts {
		var found bool
		for i, existingVolumeMount := range volumeMounts {
			if existingVolumeMount.Name == volumeMount.Name {
				volumeMounts[i] = volumeMount
				found = true
				break
			}
		}

		if !found {
			volumeMounts = append(volumeMounts, volumeMount)
		}
	}

	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ldap-" + directory.Name,
			Namespace: directory.Namespace,
			Labels:    make(map[string]string),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:        ptr.To(int32(1)),
			ServiceName:     "ldap",
			MinReadySeconds: 10,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "ldap",
					"app.kubernetes.io/instance": directory.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":     "ldap",
						"app.kubernetes.io/instance": directory.Name,
					},
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(10)),
					SecurityContext: &corev1.PodSecurityContext{
						// The default Debian OpenLDAP group.
						FSGroup: ptr.To(int64(101)),
					},
					InitContainers: []corev1.Container{
						{
							Name:  "openldap-init",
							Image: directory.Spec.Image,
							Command: []string{
								"/bootstrap.sh",
							},
							Env:          envVars,
							VolumeMounts: volumeMounts,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "openldap",
							Image: directory.Spec.Image,
							Env:   envVars,
							Ports: []corev1.ContainerPort{
								{
									Name:          "ldaps",
									ContainerPort: 636,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.IntOrString{IntVal: 636},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							VolumeMounts: volumeMounts,
							Resources:    directory.Spec.Resources,
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "certs",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  directory.Spec.CertificateSecretRef.Name,
									DefaultMode: ptr.To(int32(0o400)),
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
		},
	}

	if err := controllerutil.SetOwnerReference(directory, &sts, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference: %w", err)
	}

	for k, v := range directory.ObjectMeta.Labels {
		sts.ObjectMeta.Labels[k] = v
	}

	sts.ObjectMeta.Labels["app.kubernetes.io/name"] = "directory"
	sts.ObjectMeta.Labels["app.kubernetes.io/instance"] = directory.Name
	sts.ObjectMeta.Labels["app.kubernetes.io/managed-by"] = "ldap-operator"

	return &sts, nil
}

func (r *LDAPDirectoryReconciler) isStatefulSetReady(ctx context.Context, directory *ldapv1alpha1.LDAPDirectory) (bool, error) {
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ldap-" + directory.Name,
			Namespace: directory.Namespace,
		},
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(&sts), &sts); err != nil {
		return false, fmt.Errorf("failed to get statefulset: %w", err)
	}

	return sts.Status.ReadyReplicas == *sts.Spec.Replicas, nil
}

func (r *LDAPDirectoryReconciler) serviceTemplate(directory *ldapv1alpha1.LDAPDirectory) (*corev1.Service, error) {
	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ldap-" + directory.Name,
			Namespace: directory.Namespace,
			Labels:    make(map[string]string),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name":     "ldap",
				"app.kubernetes.io/instance": directory.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       636,
					TargetPort: intstr.FromInt(636),
					Name:       "ldaps",
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(directory, &svc, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	for k, v := range directory.ObjectMeta.Labels {
		svc.ObjectMeta.Labels[k] = v
	}

	svc.ObjectMeta.Labels["app.kubernetes.io/name"] = "directory"
	svc.ObjectMeta.Labels["app.kubernetes.io/instance"] = directory.Name
	svc.ObjectMeta.Labels["app.kubernetes.io/managed-by"] = "ldap-operator"

	return &svc, nil
}

func defaultVolumeClaimTemplates() []corev1.PersistentVolumeClaim {
	return []corev1.PersistentVolumeClaim{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "config",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("10Mi"),
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "data",
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("100Mi"),
					},
				},
			},
		},
	}
}
