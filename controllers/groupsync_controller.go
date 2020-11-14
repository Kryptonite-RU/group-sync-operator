/*


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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	userv1 "github.com/openshift/api/user/v1"
	"github.com/operator-framework/operator-lib/status"
	"github.com/prometheus/common/log"
	"github.com/redhat-cop/group-sync-operator/pkg/constants"
	"github.com/redhat-cop/group-sync-operator/pkg/syncer"
	"github.com/redhat-cop/operator-utils/pkg/util"
	"github.com/robfig/cron"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeclock "k8s.io/apimachinery/pkg/util/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redhatcopv1alpha1 "github.com/redhat-cop/group-sync-operator/api/v1alpha1"
)

var clock kubeclock.Clock = &kubeclock.RealClock{}

// GroupSyncReconciler reconciles a GroupSync object
type GroupSyncReconciler struct {
	Log logr.Logger
	util.ReconcilerBase
}

// +kubebuilder:rbac:groups=redhatcop.redhat.io,resources=groupsyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redhatcop.redhat.io,resources=groupsyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=user.openshift.io,resources=groups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *GroupSyncReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	logger := r.Log.WithValues("groupsync", req.NamespacedName)

	// Fetch the GroupSync instance
	instance := &redhatcopv1alpha1.GroupSync{}
	err := r.GetClient().Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// Get Group Sync Manager
	groupSyncMgr, err := syncer.GetGroupSyncMgr(instance, r.ReconcilerBase)

	if err != nil {
		return r.manageError(instance, err, logger)
	}

	// Set Defaults
	if changed := groupSyncMgr.SetDefaults(); changed {
		err := r.GetClient().Update(context.TODO(), instance)
		if err != nil {
			log.Error(err, "unable to update instance", "instance", instance)
			return r.manageError(instance, err, logger)
		}
		return reconcile.Result{}, nil
	}

	// Validate Providers
	if err := groupSyncMgr.Validate(); err != nil {
		return r.manageError(instance, err, logger)
	}

	// Execute Each Provider Syncer
	for _, groupSyncer := range groupSyncMgr.GroupSyncers {

		logger.Info("Beginning Sync", "Provider", groupSyncer.GetProviderName())

		// Provider Label
		providerLabel := fmt.Sprintf("%s_%s", instance.Name, groupSyncer.GetProviderName())

		// Initialize Connection
		if err := groupSyncer.Bind(); err != nil {
			return r.manageError(instance, err, logger)
		}

		// Perform Sync
		groups, err := groupSyncer.Sync()

		if err != nil {
			logger.Error(err, "Failed to Complete Sync", "Provider", groupSyncer.GetProviderName())
			return r.manageError(instance, err, logger)
		}

		updatedGroups := 0

		for _, group := range groups {

			ocpGroup := &userv1.Group{}
			err := r.GetClient().Get(context.TODO(), types.NamespacedName{Name: group.Name, Namespace: ""}, ocpGroup)

			if apierrors.IsNotFound(err) {

				ocpGroup = &userv1.Group{}
				ocpGroup.Name = group.Name

			} else if err != nil {
				return r.manageError(instance, err, logger)
			} else {
				// Verify this group is not managed by another provider
				if groupProviderLabel, exists := ocpGroup.Labels[constants.SyncProvider]; !exists || (groupProviderLabel != providerLabel) {
					log.Info("Group Provider Label Did Not Match Expected Provider Label", "Group Name", ocpGroup.Name, "Expected Label", providerLabel, "Found Label", groupProviderLabel)
					continue
				}
			}

			// Copy Annotations/Labels
			ocpGroupLabels := map[string]string{}
			ocpGroupAnnotations := map[string]string{}

			if group.GetAnnotations() != nil {
				ocpGroupAnnotations = group.GetAnnotations()
			}

			if group.GetLabels() != nil {
				ocpGroupLabels = group.GetLabels()
			}
			ocpGroup.SetLabels(ocpGroupLabels)
			ocpGroup.SetAnnotations(ocpGroupAnnotations)

			// Add Label for new resource
			ocpGroup.Labels[constants.SyncProvider] = providerLabel

			// Add Gloabl Annotations/Labels
			ocpGroup.Annotations[constants.SyncTimestamp] = ISO8601(time.Now())

			ocpGroup.Users = group.Users

			err = r.CreateOrUpdateResource(instance, "", ocpGroup)

			if err != nil {
				log.Error(err, "Failed to Create or Update OpenShift Group")
				return r.manageError(instance, err, logger)
			}

			updatedGroups++

		}

		logger.Info("Sync Completed Successfully", "Provider", groupSyncer.GetProviderName(), "Groups Created or Updated", updatedGroups)

	}

	instance.Status.LastSyncSuccessTime = &metav1.Time{Time: clock.Now()}

	successResult, err := r.manageSuccess(instance, logger)

	if err == nil && instance.Spec.Schedule != "" {
		sched, _ := cron.ParseStandard(instance.Spec.Schedule)

		currentTime := time.Now()
		nextScheduledTime := sched.Next(currentTime)
		successResult.RequeueAfter = nextScheduledTime.Sub(currentTime)
	}

	return successResult, err
}

func (r *GroupSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redhatcopv1alpha1.GroupSync{}).
		WithEventFilter(util.ResourceGenerationOrFinalizerChangedPredicate{}).
		Complete(r)
}

func (r *GroupSyncReconciler) manageSuccess(instance *redhatcopv1alpha1.GroupSync, logger logr.Logger) (ctrl.Result, error) {
	condition := status.Condition{
		Type:    status.ConditionType("groupsync"),
		Status:  corev1.ConditionTrue,
		Reason:  status.ConditionReason("synchronization succeeded"),
		Message: "group synchronization has succeeded",
	}

	instance.Status.Conditions.SetCondition(condition)

	err := r.GetClient().Status().Update(context.Background(), instance)
	if err != nil {
		log.Error(err, "unable to update status")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *GroupSyncReconciler) manageError(instance *redhatcopv1alpha1.GroupSync, err error, logger logr.Logger) (ctrl.Result, error) {
	r.GetRecorder().Event(instance, "Warning", "GroupSyncError", err.Error())

	condition := status.Condition{
		Type:    status.ConditionType("groupsync"),
		Status:  corev1.ConditionTrue,
		Reason:  status.ConditionReason("synchronization error"),
		Message: err.Error(),
	}

	instance.Status.Conditions.SetCondition(condition)

	updateErr := r.GetClient().Status().Update(context.Background(), instance)
	if err != nil {
		log.Error(updateErr, "unable to update status")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, err
}

func ISO8601(t time.Time) string {
	var tz string
	if zone, offset := t.Zone(); zone == "UTC" {
		tz = "Z"
	} else {
		tz = fmt.Sprintf("%03d00", offset/3600)
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d%s",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), tz)
}
