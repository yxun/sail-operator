// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remoteistio

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/istio-ecosystem/sail-operator/api/v1alpha1"
	"github.com/istio-ecosystem/sail-operator/pkg/config"
	"github.com/istio-ecosystem/sail-operator/pkg/errlist"
	"github.com/istio-ecosystem/sail-operator/pkg/kube"
	"github.com/istio-ecosystem/sail-operator/pkg/reconciler"
	"github.com/istio-ecosystem/sail-operator/pkg/revision"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"istio.io/istio/pkg/ptr"
)

// Reconciler reconciles a RemoteIstio object
type Reconciler struct {
	Config config.ReconcilerConfig
	client.Client
	Scheme *runtime.Scheme
}

func NewReconciler(cfg config.ReconcilerConfig, client client.Client, scheme *runtime.Scheme) *Reconciler {
	return &Reconciler{
		Config: cfg,
		Client: client,
		Scheme: scheme,
	}
}

// +kubebuilder:rbac:groups=sailoperator.io,resources=remoteistios,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sailoperator.io,resources=remoteistios/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sailoperator.io,resources=remoteistios/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *Reconciler) Reconcile(ctx context.Context, istio *v1alpha1.RemoteIstio) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconciling")
	result, reconcileErr := r.doReconcile(ctx, istio)

	log.Info("Reconciliation done. Updating status.")
	statusErr := r.updateStatus(ctx, istio, reconcileErr)

	return result, errors.Join(reconcileErr, statusErr)
}

// doReconcile is the function that actually reconciles the Istio object. Any error reported by this
// function should get reported in the status of the Istio object by the caller.
func (r *Reconciler) doReconcile(ctx context.Context, istio *v1alpha1.RemoteIstio) (result ctrl.Result, err error) {
	if err := validate(istio); err != nil {
		return ctrl.Result{}, err
	}

	if err = r.reconcileActiveRevision(ctx, istio); err != nil {
		return ctrl.Result{}, err
	}

	return revision.PruneInactive(ctx, r.Client, istio.UID, getActiveRevisionName(istio), getPruningGracePeriod(istio))
}

func validate(istio *v1alpha1.RemoteIstio) error {
	if istio.Spec.Version == "" {
		return reconciler.NewValidationError("spec.version not set")
	}
	if istio.Spec.Namespace == "" {
		return reconciler.NewValidationError("spec.namespace not set")
	}
	return nil
}

func (r *Reconciler) reconcileActiveRevision(ctx context.Context, istio *v1alpha1.RemoteIstio) error {
	values, err := revision.ComputeValues(
		istio.Spec.Values, istio.Spec.Namespace, istio.Spec.Version,
		r.Config.Platform, r.Config.DefaultProfile, istio.Spec.Profile,
		r.Config.ResourceDirectory, getActiveRevisionName(istio))
	if err != nil {
		return err
	}

	return revision.CreateOrUpdate(ctx, r.Client,
		getActiveRevisionName(istio),
		v1alpha1.IstioRevisionTypeRemote,
		istio.Spec.Version, istio.Spec.Namespace, values,
		metav1.OwnerReference{
			APIVersion:         v1alpha1.GroupVersion.String(),
			Kind:               v1alpha1.RemoteIstioKind,
			Name:               istio.Name,
			UID:                istio.UID,
			Controller:         ptr.Of(true),
			BlockOwnerDeletion: ptr.Of(true),
		})
}

func getPruningGracePeriod(istio *v1alpha1.RemoteIstio) time.Duration {
	strategy := istio.Spec.UpdateStrategy
	period := int64(v1alpha1.DefaultRevisionDeletionGracePeriodSeconds)
	if strategy != nil && strategy.InactiveRevisionDeletionGracePeriodSeconds != nil {
		period = *strategy.InactiveRevisionDeletionGracePeriodSeconds
	}
	if period < v1alpha1.MinRevisionDeletionGracePeriodSeconds {
		period = v1alpha1.MinRevisionDeletionGracePeriodSeconds
	}
	return time.Duration(period) * time.Second
}

func (r *Reconciler) getActiveRevision(ctx context.Context, istio *v1alpha1.RemoteIstio) (v1alpha1.IstioRevision, error) {
	rev := v1alpha1.IstioRevision{}
	err := r.Client.Get(ctx, getActiveRevisionKey(istio), &rev)
	if err != nil {
		return rev, fmt.Errorf("get failed: %w", err)
	}
	return rev, nil
}

func getActiveRevisionKey(istio *v1alpha1.RemoteIstio) types.NamespacedName {
	return types.NamespacedName{
		Name: getActiveRevisionName(istio),
	}
}

func getActiveRevisionName(istio *v1alpha1.RemoteIstio) string {
	var strategy v1alpha1.UpdateStrategyType
	if istio.Spec.UpdateStrategy != nil {
		strategy = istio.Spec.UpdateStrategy.Type
	}

	switch strategy {
	default:
		fallthrough
	case v1alpha1.UpdateStrategyTypeInPlace:
		return istio.Name
	case v1alpha1.UpdateStrategyTypeRevisionBased:
		return istio.Name + "-" + strings.ReplaceAll(istio.Spec.Version, ".", "-")
	}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			LogConstructor: func(req *reconcile.Request) logr.Logger {
				log := mgr.GetLogger().WithName("ctrlr").WithName("remoteistio")
				if req != nil {
					log = log.WithValues("RemoteIstio", req.Name)
				}
				return log
			},
		}).
		For(&v1alpha1.RemoteIstio{}).
		Owns(&v1alpha1.IstioRevision{}).
		Complete(reconciler.NewStandardReconciler[*v1alpha1.RemoteIstio](r.Client, r.Reconcile))
}

func (r *Reconciler) determineStatus(ctx context.Context, istio *v1alpha1.RemoteIstio, reconcileErr error) (v1alpha1.RemoteIstioStatus, error) {
	var errs errlist.Builder
	status := *istio.Status.DeepCopy()
	status.ObservedGeneration = istio.Generation

	// set Reconciled and Ready conditions
	if reconcileErr != nil {
		status.SetCondition(v1alpha1.RemoteIstioCondition{
			Type:    v1alpha1.RemoteIstioConditionReconciled,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.RemoteIstioReasonReconcileError,
			Message: reconcileErr.Error(),
		})
		status.SetCondition(v1alpha1.RemoteIstioCondition{
			Type:    v1alpha1.RemoteIstioConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  v1alpha1.RemoteIstioReasonReconcileError,
			Message: "cannot determine readiness due to reconciliation error",
		})
		status.State = v1alpha1.RemoteIstioReasonReconcileError
	} else {
		status.ActiveRevisionName = getActiveRevisionName(istio)
		rev, err := r.getActiveRevision(ctx, istio)
		if apierrors.IsNotFound(err) {
			revisionNotFound := func(conditionType v1alpha1.RemoteIstioConditionType) v1alpha1.RemoteIstioCondition {
				return v1alpha1.RemoteIstioCondition{
					Type:    conditionType,
					Status:  metav1.ConditionFalse,
					Reason:  v1alpha1.RemoteIstioReasonRevisionNotFound,
					Message: "active IstioRevision not found",
				}
			}

			status.SetCondition(revisionNotFound(v1alpha1.RemoteIstioConditionReconciled))
			status.SetCondition(revisionNotFound(v1alpha1.RemoteIstioConditionReady))
			status.State = v1alpha1.RemoteIstioReasonRevisionNotFound
		} else if err == nil {
			status.SetCondition(convertCondition(rev.Status.GetCondition(v1alpha1.IstioRevisionConditionReconciled)))
			status.SetCondition(convertCondition(rev.Status.GetCondition(v1alpha1.IstioRevisionConditionReady)))
			status.State = convertConditionReason(rev.Status.State)
		} else {
			activeRevisionGetFailed := func(conditionType v1alpha1.RemoteIstioConditionType) v1alpha1.RemoteIstioCondition {
				return v1alpha1.RemoteIstioCondition{
					Type:    conditionType,
					Status:  metav1.ConditionUnknown,
					Reason:  v1alpha1.RemoteIstioReasonFailedToGetActiveRevision,
					Message: fmt.Sprintf("failed to get active IstioRevision: %s", err),
				}
			}
			status.SetCondition(activeRevisionGetFailed(v1alpha1.RemoteIstioConditionReconciled))
			status.SetCondition(activeRevisionGetFailed(v1alpha1.RemoteIstioConditionReady))
			status.State = v1alpha1.RemoteIstioReasonFailedToGetActiveRevision
			errs.Add(fmt.Errorf("failed to get active IstioRevision: %w", err))
		}
	}

	// count the ready, in-use, and total revisions
	if revs, err := revision.ListOwned(ctx, r.Client, istio.UID); err == nil {
		status.Revisions.Total = int32(len(revs))
		status.Revisions.Ready = 0
		status.Revisions.InUse = 0
		for _, rev := range revs {
			if rev.Status.GetCondition(v1alpha1.IstioRevisionConditionReady).Status == metav1.ConditionTrue {
				status.Revisions.Ready++
			}
			if rev.Status.GetCondition(v1alpha1.IstioRevisionConditionInUse).Status == metav1.ConditionTrue {
				status.Revisions.InUse++
			}
		}
	} else {
		status.Revisions.Total = -1
		status.Revisions.Ready = -1
		status.Revisions.InUse = -1
		errs.Add(err)
	}
	return status, errs.Error()
}

func (r *Reconciler) updateStatus(ctx context.Context, istio *v1alpha1.RemoteIstio, reconcileErr error) error {
	var errs errlist.Builder
	status, err := r.determineStatus(ctx, istio, reconcileErr)
	if err != nil {
		errs.Add(fmt.Errorf("failed to determine status: %w", err))
	}

	if !reflect.DeepEqual(istio.Status, status) {
		if err := r.Client.Status().Patch(ctx, istio, kube.NewStatusPatch(status)); err != nil {
			errs.Add(fmt.Errorf("failed to patch status: %w", err))
		}
	}
	return errs.Error()
}

func convertCondition(condition v1alpha1.IstioRevisionCondition) v1alpha1.RemoteIstioCondition {
	return v1alpha1.RemoteIstioCondition{
		Type:    convertConditionType(condition),
		Status:  condition.Status,
		Reason:  convertConditionReason(condition.Reason),
		Message: condition.Message,
	}
}

func convertConditionType(condition v1alpha1.IstioRevisionCondition) v1alpha1.RemoteIstioConditionType {
	switch condition.Type {
	case v1alpha1.IstioRevisionConditionReconciled:
		return v1alpha1.RemoteIstioConditionReconciled
	case v1alpha1.IstioRevisionConditionReady:
		return v1alpha1.RemoteIstioConditionReady
	default:
		panic(fmt.Sprintf("can't convert IstioRevisionConditionType: %s", condition.Type))
	}
}

func convertConditionReason(reason v1alpha1.IstioRevisionConditionReason) v1alpha1.RemoteIstioConditionReason {
	switch reason {
	case "":
		return ""
	case v1alpha1.IstioRevisionReasonRemoteIstiodNotReady:
		return v1alpha1.RemoteIstioReasonIstiodNotReady
	case v1alpha1.IstioRevisionReasonHealthy:
		return v1alpha1.RemoteIstioReasonHealthy
	case v1alpha1.IstioRevisionReasonReadinessCheckFailed:
		return v1alpha1.RemoteIstioReasonReadinessCheckFailed
	case v1alpha1.IstioRevisionReasonReconcileError:
		return v1alpha1.RemoteIstioReasonReconcileError
	default:
		panic(fmt.Sprintf("can't convert IstioRevisionConditionReason: %s", reason))
	}
}
