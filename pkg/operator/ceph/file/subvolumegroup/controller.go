/*
Copyright 2021 The Rook Authors. All rights reserved.

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

// Package subvolumegroup to manage CephFS subvolume groups
package subvolumegroup

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/util/exec"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/coreos/pkg/capnslog"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/ceph/reporting"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	controllerName = "ceph-fs-subvolumegroup-controller"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", controllerName)

var cephFilesystemSubVolumeGroup = reflect.TypeOf(cephv1.CephFilesystemSubVolumeGroup{}).Name()

// Sets the type meta for the controller main object
var controllerTypeMeta = metav1.TypeMeta{
	Kind:       cephFilesystemSubVolumeGroup,
	APIVersion: fmt.Sprintf("%s/%s", cephv1.CustomResourceGroup, cephv1.Version),
}

// ReconcileCephFilesystemSubVolumeGroup reconciles a CephFilesystemSubVolumeGroup object
type ReconcileCephFilesystemSubVolumeGroup struct {
	client           client.Client
	scheme           *runtime.Scheme
	context          *clusterd.Context
	clusterInfo      *cephclient.ClusterInfo
	opManagerContext context.Context
}

// Add creates a new CephFilesystemSubVolumeGroup Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, context *clusterd.Context, opManagerContext context.Context, opConfig opcontroller.OperatorConfig) error {
	return add(mgr, newReconciler(mgr, context, opManagerContext))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, context *clusterd.Context, opManagerContext context.Context) reconcile.Reconciler {
	return &ReconcileCephFilesystemSubVolumeGroup{
		client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		context:          context,
		opManagerContext: opManagerContext,
	}
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	logger.Info("successfully started")

	// Watch for changes on the CephFilesystemSubVolumeGroup CRD object
	err = c.Watch(&source.Kind{Type: &cephv1.CephFilesystemSubVolumeGroup{TypeMeta: controllerTypeMeta}}, &handler.EnqueueRequestForObject{}, opcontroller.WatchControllerPredicate())
	if err != nil {
		return err
	}

	return nil
}

// Reconcile reads that state of the cluster for a CephFilesystemSubVolumeGroup object and makes changes based on the state read
// and what is in the CephFilesystemSubVolumeGroup.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileCephFilesystemSubVolumeGroup) Reconcile(context context.Context, request reconcile.Request) (reconcile.Result, error) {
	// workaround because the rook logging mechanism is not compatible with the controller-runtime logging interface
	reconcileResponse, err := r.reconcile(request)
	if err != nil {
		logger.Errorf("failed to reconcile %v", err)
	}

	return reconcileResponse, err
}

func (r *ReconcileCephFilesystemSubVolumeGroup) reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the CephFilesystemSubVolumeGroup instance
	cephFilesystemSubVolumeGroup := &cephv1.CephFilesystemSubVolumeGroup{}
	err := r.client.Get(r.opManagerContext, request.NamespacedName, cephFilesystemSubVolumeGroup)
	if err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("cephFilesystemSubVolumeGroup resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.Wrap(err, "failed to get cephFilesystemSubVolumeGroup")
	}

	// Set a finalizer so we can do cleanup before the object goes away
	err = opcontroller.AddFinalizerIfNotPresent(r.opManagerContext, r.client, cephFilesystemSubVolumeGroup)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to add finalizer")
	}

	// The CR was just created, initializing status fields
	if cephFilesystemSubVolumeGroup.Status == nil {
		r.updateStatus(r.client, request.NamespacedName, cephv1.ConditionProgressing)
	}

	// Make sure a CephCluster is present otherwise do nothing
	_, isReadyToReconcile, cephClusterExists, reconcileResponse := opcontroller.IsReadyToReconcile(r.opManagerContext, r.client, request.NamespacedName, controllerName)
	if !isReadyToReconcile {
		// This handles the case where the Ceph Cluster is gone and we want to delete that CR
		// We skip the deleteSubVolumeGroup() function since everything is gone already
		//
		// Also, only remove the finalizer if the CephCluster is gone
		// If not, we should wait for it to be ready
		// This handles the case where the operator is not ready to accept Ceph command but the cluster exists
		if !cephFilesystemSubVolumeGroup.GetDeletionTimestamp().IsZero() && !cephClusterExists {
			// Remove finalizer
			err = opcontroller.RemoveFinalizer(r.opManagerContext, r.client, cephFilesystemSubVolumeGroup)
			if err != nil {
				return opcontroller.ImmediateRetryResult, errors.Wrap(err, "failed to remove finalizer")
			}

			// Return and do not requeue. Successful deletion.
			return reconcile.Result{}, nil
		}
		return reconcileResponse, nil
	}

	// Populate clusterInfo during each reconcile
	r.clusterInfo, _, _, err = mon.LoadClusterInfo(r.context, r.opManagerContext, request.NamespacedName.Namespace)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to populate cluster info")
	}
	r.clusterInfo.Context = r.opManagerContext

	// DELETE: the CR was deleted
	if !cephFilesystemSubVolumeGroup.GetDeletionTimestamp().IsZero() {
		logger.Debugf("deleting subvolume group %q", cephFilesystemSubVolumeGroup.Name)
		err := r.deleteSubVolumeGroup(cephFilesystemSubVolumeGroup)
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to delete ceph ceph filesystem subvolume group %q", cephFilesystemSubVolumeGroup.Name)
		}

		// Remove finalizer
		err = opcontroller.RemoveFinalizer(r.opManagerContext, r.client, cephFilesystemSubVolumeGroup)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to remove finalizer")
		}

		// Return and do not requeue. Successful deletion.
		return reconcile.Result{}, nil
	}

	// Build the NamespacedName to fetch the Filesystem and make sure it exists, if not we cannot
	// create the subvolumegroup
	cephFilesystem := &cephv1.CephFilesystem{}
	cephFilesystemNamespacedName := types.NamespacedName{Name: cephFilesystemSubVolumeGroup.Spec.FilesystemName, Namespace: request.Namespace}
	err = r.client.Get(r.opManagerContext, cephFilesystemNamespacedName, cephFilesystem)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return reconcile.Result{}, errors.Wrapf(err, "failed to fetch ceph filesystem %q, cannot create subvolumegroup %q", cephFilesystemSubVolumeGroup.Spec.FilesystemName, cephFilesystemSubVolumeGroup.Name)
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.Wrap(err, "failed to get cephFilesystemSubVolumeGroup")
	}

	// If the CephFilesystem is not ready to accept commands, we should wait for it to be ready
	if cephFilesystem.Status.Phase != cephv1.ConditionReady {
		// We know the CR is present so it should a matter of second for it to become ready
		return reconcile.Result{Requeue: true, RequeueAfter: 10 * time.Second}, errors.Wrapf(err, "failed to fetch ceph filesystem %q, cannot create subvolumegroup %q", cephFilesystemSubVolumeGroup.Spec.FilesystemName, cephFilesystemSubVolumeGroup.Name)
	}

	// Create or Update ceph filesystem subvolume group
	err = r.createOrUpdateSubVolumeGroup(cephFilesystemSubVolumeGroup)
	if err != nil {
		if strings.Contains(err.Error(), opcontroller.UninitializedCephConfigError) {
			logger.Info(opcontroller.OperatorNotInitializedMessage)
			return opcontroller.WaitForRequeueIfOperatorNotInitialized, nil
		}
		r.updateStatus(r.client, request.NamespacedName, cephv1.ConditionFailure)
		return reconcile.Result{}, errors.Wrapf(err, "failed to create or update ceph filesystem subvolume group %q", cephFilesystemSubVolumeGroup.Name)
	}

	// Success! Let's update the status
	r.updateStatus(r.client, request.NamespacedName, cephv1.ConditionReady)

	// Return and do not requeue
	logger.Debug("done reconciling")
	return reconcile.Result{}, nil
}

// Create the ceph filesystem subvolume group
func (r *ReconcileCephFilesystemSubVolumeGroup) createOrUpdateSubVolumeGroup(cephFilesystemSubVolumeGroup *cephv1.CephFilesystemSubVolumeGroup) error {
	logger.Infof("creating ceph filesystem subvolume group %s in namespace %s", cephFilesystemSubVolumeGroup.Name, cephFilesystemSubVolumeGroup.Namespace)

	err := cephclient.CreateCephFSSubVolumeGroup(r.context, r.clusterInfo, cephFilesystemSubVolumeGroup.Spec.FilesystemName, cephFilesystemSubVolumeGroup.Name)
	if err != nil {
		return errors.Wrapf(err, "failed to create ceph filesystem subvolume group %q", cephFilesystemSubVolumeGroup.Name)
	}

	return nil
}

// Delete the ceph filesystem subvolume group
func (r *ReconcileCephFilesystemSubVolumeGroup) deleteSubVolumeGroup(cephFilesystemSubVolumeGroup *cephv1.CephFilesystemSubVolumeGroup) error {
	logger.Infof("deleting ceph filesystem subvolume group object %q", cephFilesystemSubVolumeGroup.Name)
	if err := cephclient.DeleteCephFSSubVolumeGroup(r.context, r.clusterInfo, cephFilesystemSubVolumeGroup.Name, cephFilesystemSubVolumeGroup.Spec.FilesystemName); err != nil {
		code, ok := exec.ExitStatus(err)
		// If the subvolumegroup has subvolumes the command will fail with:
		// Error ENOTEMPTY: error in rmdir /volumes/csi
		if ok && code != int(syscall.ENOTEMPTY) {
			return errors.Wrapf(err, "failed to delete ceph filesystem subvolume group %q, remove the subvolumes first.", cephFilesystemSubVolumeGroup.Name)
		}

		return errors.Wrapf(err, "failed to delete ceph filesystem subvolume group %q", cephFilesystemSubVolumeGroup.Name)
	}

	logger.Infof("deleted ceph filesystem subvolume group %q", cephFilesystemSubVolumeGroup.Name)
	return nil
}

// updateStatus updates an object with a given status
func (r *ReconcileCephFilesystemSubVolumeGroup) updateStatus(client client.Client, name types.NamespacedName, status cephv1.ConditionType) {
	cephFilesystemSubVolumeGroup := &cephv1.CephFilesystemSubVolumeGroup{}
	if err := client.Get(r.opManagerContext, name, cephFilesystemSubVolumeGroup); err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("CephFilesystemSubVolumeGroup resource not found. Ignoring since object must be deleted.")
			return
		}
		logger.Warningf("failed to retrieve ceph ceph filesystem subvolume group %q to update status to %q. %v", name, status, err)
		return
	}
	if cephFilesystemSubVolumeGroup.Status == nil {
		cephFilesystemSubVolumeGroup.Status = &cephv1.CephFilesystemSubVolumeGroupStatus{}
	}

	cephFilesystemSubVolumeGroup.Status.Phase = status
	if err := reporting.UpdateStatus(client, cephFilesystemSubVolumeGroup); err != nil {
		logger.Errorf("failed to set ceph ceph filesystem subvolume group %q status to %q. %v", name, status, err)
		return
	}
	logger.Debugf("ceph ceph filesystem subvolume group %q status updated to %q", name, status)
}