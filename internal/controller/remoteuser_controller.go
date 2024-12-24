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
	"slices"

	syngit "github.com/syngit-org/syngit/api/v1beta2"
	gitlab "gitlab.com/gitlab-org/api/client-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// RemoteUserReconciler reconciles a RemoteUser object
type RemoteUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type RemoteUserChecker struct {
	remoteUser syngit.RemoteUser
	secret     corev1.Secret
}

// +kubebuilder:rbac:groups=syngit.io,resources=remoteusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=syngit.io,resources=remoteusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=syngit.io,resources=remoteusers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *RemoteUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// Get the RemoteUser Object
	var remoteUser syngit.RemoteUser
	if err := r.Get(ctx, req.NamespacedName, &remoteUser); err != nil {
		// does not exists -> deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Log.Info("Reconcile request",
		"resource", "remoteuser",
		"namespace", remoteUser.Namespace,
		"name", remoteUser.Name,
	)

	remoteUserChecker := RemoteUserChecker{remoteUser: *remoteUser.DeepCopy()}

	var secret corev1.Secret
	namespacedNameSecret := types.NamespacedName{Namespace: req.Namespace, Name: remoteUser.Spec.SecretRef.Name}
	if err := r.Get(ctx, namespacedNameSecret, &secret); err != nil {
		fmt.Println(err)
		remoteUserChecker.secret = corev1.Secret{}
	} else {
		remoteUserChecker.secret = secret
	}

	remoteUserChecker.testConnection()

	remoteUser.Status.Conditions = remoteUserChecker.remoteUser.Status.Conditions
	_ = r.Status().Update(ctx, &remoteUser)

	return ctrl.Result{}, nil
}

func (ruc *RemoteUserChecker) testConnection() {
	conditions := ruc.remoteUser.Status.DeepCopy().Conditions

	if ruc.remoteUser.Annotations["gitlab.syngit.io/auth.test"] != "true" {
		ruc.remoteUser.Status.Conditions = typeBasedConditionRemover(conditions, "Authenticated")
	} else {
		if len(ruc.secret.Data) != 0 {
			client, err := gitlab.NewClient(string(ruc.secret.Data["password"]))
			if err != nil {
				user, _, err := client.Users.CurrentUser()
				if err != nil {
					condition := metav1.Condition{
						Type:               "Authenticated",
						Status:             metav1.ConditionFalse,
						Reason:             "AuthenticationFailed",
						Message:            err.Error(),
						LastTransitionTime: metav1.Now(),
					}
					ruc.remoteUser.Status.Conditions = typeBasedConditionUpdater(conditions, condition)
				} else {
					condition := metav1.Condition{
						Type:               "Authenticated",
						Status:             metav1.ConditionTrue,
						Reason:             "AuthenticationSucceded",
						Message:            fmt.Sprintf("Authentication was successful with the user %s", user.Username),
						LastTransitionTime: metav1.Now(),
					}
					ruc.remoteUser.Status.Conditions = typeBasedConditionUpdater(conditions, condition)
				}
			}
		}
	}
}

func typeBasedConditionUpdater(conditions []metav1.Condition, condition metav1.Condition) []metav1.Condition {
	conditions = typeBasedConditionRemover(conditions, condition.Type)
	conditions = append(conditions, condition)

	return conditions
}

func typeBasedConditionRemover(conditions []metav1.Condition, typeKind string) []metav1.Condition {
	removeIndex := -1
	for i, statusCondition := range conditions {
		if typeKind == statusCondition.Type {
			removeIndex = i
		}
	}
	if removeIndex != -1 {
		conditions = slices.Delete(conditions, removeIndex, removeIndex+1)
	}

	return conditions
}

func (r *RemoteUserReconciler) findObjectsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	attachedRemoteUsers := &syngit.RemoteUserList{}
	listOps := &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(secretRefField, secret.GetName()),
		Namespace:     secret.GetNamespace(),
	}
	err := r.List(ctx, attachedRemoteUsers, listOps)
	if err != nil {
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, len(attachedRemoteUsers.Items))
	for i, item := range attachedRemoteUsers.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      item.GetName(),
				Namespace: item.GetNamespace(),
			},
		}
	}
	return requests
}

const (
	secretRefField = ".spec.secretRef.name"
)

// SetupWithManager sets up the controller with the Manager.
func (r *RemoteUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&syngit.RemoteUser{}).
		Named("remoteuser").
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForSecret),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
