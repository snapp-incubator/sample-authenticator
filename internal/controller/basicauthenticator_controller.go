/*
Copyright 2023.

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
	authenticatorv1alpha1 "github.com/sinamna/BasicAthenticator/api/v1alpha1"
	"github.com/sinamna/BasicAthenticator/internal/config"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// BasicAuthenticatorReconciler reconciles a BasicAuthenticator object
type BasicAuthenticatorReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	CustomConfig *config.CustomConfig
}

//+kubebuilder:rbac:groups=authenticator.snappcloud.io,resources=basicauthenticators,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=authenticator.snappcloud.io,resources=basicauthenticators/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=authenticator.snappcloud.io,resources=basicauthenticators/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:resources=secret,verbs=get;list;watch;create;update;patch;delete

func (r *BasicAuthenticatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("reconcile triggered")
	logger.Info(req.String())
	basicAuthenticator := &authenticatorv1alpha1.BasicAuthenticator{}
	err := r.Get(ctx, req.NamespacedName, basicAuthenticator)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get BasicAuthenticator")
		return ctrl.Result{}, err
	}

	if err := r.Get(ctx, req.NamespacedName, basicAuthenticator); err != nil {
		logger.Error(err, "failed to refetch")
		return ctrl.Result{}, err
	}

	credentialName := basicAuthenticator.Spec.CredentialsSecretRef
	var credentialSecret corev1.Secret
	if credentialName == "" {
		//create secret
		newSecret := r.CreateCredentials(basicAuthenticator)
		err = r.Get(ctx, types.NamespacedName{Name: newSecret.Name, Namespace: newSecret.Namespace}, &credentialSecret)
		if errors.IsNotFound(err) {
			// update basic auth
			err := r.Create(ctx, newSecret)
			if err != nil {
				logger.Error(err, "failed to create new secret")
				return ctrl.Result{Requeue: true}, err
			}

			credentialName = newSecret.Name
			credentialSecret = *newSecret
			basicAuthenticator.Spec.CredentialsSecretRef = credentialName
			err = r.Update(ctx, basicAuthenticator)
			if err != nil {
				logger.Error(err, "failed to updated basic authenticator")
				return ctrl.Result{}, err
			}

			err = r.Get(ctx, req.NamespacedName, basicAuthenticator)
			if err != nil {
				logger.Error(err, "failed to refetch basic authenticator")
				return ctrl.Result{}, err
			}
		} else {
			return ctrl.Result{Requeue: true}, nil
		}
	} else {
		err := r.Get(ctx, types.NamespacedName{Name: credentialName, Namespace: basicAuthenticator.Namespace}, &credentialSecret)
		if err != nil {
			logger.Error(err, "failed to fetch secret")
			return ctrl.Result{}, err
		}
	}

	nginxConfig := r.CreateNginxConfigmap(basicAuthenticator)
	var foundConfigmap corev1.ConfigMap
	err = r.Get(ctx, types.NamespacedName{Name: nginxConfig.Name, Namespace: basicAuthenticator.Namespace}, &foundConfigmap)
	if errors.IsNotFound(err) {
		if err := ctrl.SetControllerReference(basicAuthenticator, nginxConfig, r.Scheme); err != nil {
			logger.Error(err, "failed to set configmap owner")
			return ctrl.Result{}, err
		}
		err := r.Create(ctx, nginxConfig)
		if err != nil {
			logger.Error(err, "failed to create new configmap")
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "failed to fetch configmap")
		return ctrl.Result{}, err
	} else {
		if !reflect.DeepEqual(nginxConfig.Data, foundConfigmap.Data) {
			logger.Info("updating configmap")
			foundConfigmap.Data = nginxConfig.Data
			err := r.Update(ctx, &foundConfigmap)
			if err != nil {
				logger.Error(err, "failed to update configmap")
				return ctrl.Result{}, err
			}
		}
	}

	isSidecar := basicAuthenticator.Spec.Type == "sidecar"

	if isSidecar {
		deploymentsToUpdate, err := r.Injector(ctx, basicAuthenticator, foundConfigmap.Name, credentialName)
		if err != nil {
			logger.Error(err, "failed to inject into deployments")
			return ctrl.Result{}, err
		}
		for _, deploy := range deploymentsToUpdate {
			err := r.Update(ctx, deploy)
			if err != nil {
				logger.Error(err, "failed to update injected deployments")
				return ctrl.Result{}, err
			}
		}
	} else {
		newDeployment := r.CreateNginxDeployment(basicAuthenticator, foundConfigmap.Name, credentialName)
		foundDeployment := &appv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: newDeployment.Name, Namespace: basicAuthenticator.Namespace}, foundDeployment)
		if errors.IsNotFound(err) {
			if err := ctrl.SetControllerReference(basicAuthenticator, newDeployment, r.Scheme); err != nil {
				logger.Error(err, "failed to set deployment owner")
				return ctrl.Result{}, err
			}
			//create deployment
			err := r.Create(ctx, newDeployment)
			if err != nil {
				logger.Error(err, "failed to create new deployment")
				return ctrl.Result{Requeue: true}, err
			}
			logger.Info("created deployment")

			return ctrl.Result{Requeue: true}, nil
		} else if err != nil {
			if err != nil {
				logger.Error(err, "failed to fetch deployment")
				return ctrl.Result{}, err
			}
		} else {
			//update deployment
			if !reflect.DeepEqual(newDeployment.Spec, foundDeployment.Spec) {
				logger.Info("updating deployment")
				foundDeployment.Spec = newDeployment.Spec
				err := r.Update(ctx, foundDeployment)
				if err != nil {
					logger.Error(err, "failed to update deployment")
					return ctrl.Result{}, err
				}
			}
			logger.Info("updating ready replicas")
			basicAuthenticator.Status.ReadyReplicas = int(foundDeployment.Status.ReadyReplicas)
			err := r.Status().Update(ctx, basicAuthenticator)
			if err != nil {
				logger.Error(err, "failed to update basic authenticator status")
				return ctrl.Result{}, err
			}

		}

	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BasicAuthenticatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&authenticatorv1alpha1.BasicAuthenticator{}).
		Owns(&appv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
