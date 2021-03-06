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
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	util "github.com/openstack-k8s-operators/lib-common/pkg/util"
	databasev1beta1 "github.com/openstack-k8s-operators/mariadb-operator/api/v1beta1"
	mariadb "github.com/openstack-k8s-operators/mariadb-operator/pkg"
)

// MariaDBDatabaseReconciler reconciles a MariaDBDatabase object
type MariaDBDatabaseReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	Log     logr.Logger
	Scheme  *runtime.Scheme
}

// +kubebuilder:rbac:groups=database.openstack.org,resources=mariadbdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.openstack.org,resources=mariadbdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.openstack.org,resources=mariadbs/status,verbs=get;list
// +kubebuilder:rbac:groups=database.openstack.org,resources=mariadbs/status,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;create;update;delete;

// Reconcile reconcile mariadbdatabase API requests
func (r *MariaDBDatabaseReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("mariadbdatabase", req.NamespacedName)

	// Fetch the MariaDBDatabase instance
	instance := &databasev1beta1.MariaDBDatabase{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		// Error reading the object - requeue the request.
		// ignore not found errors, since they can't be fixed by an immediate
		// requeue, and we can get them on deleted requests which we now
		// handle using finalizer.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Fetch the MariaDB instance from which we'll pull the credentials
	db := &databasev1beta1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.ObjectMeta.Labels["dbName"],
			Namespace: req.Namespace,
		},
	}
	objectKey, err := client.ObjectKeyFromObject(db)
	err = r.Client.Get(context.TODO(), objectKey, db)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			r.Log.Info("No DB found for label 'dbName'.")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: time.Second * 10}, err
	}

	finalizerName := "mariadb-" + instance.Name
	// if deletion timestamp is set on the instance object, the CR got deleted
	if instance.ObjectMeta.DeletionTimestamp.IsZero() {
		// if it is a new instance, add the finalizer
		if !controllerutil.ContainsFinalizer(instance, finalizerName) {
			controllerutil.AddFinalizer(instance, finalizerName)
			err = r.Client.Update(context.TODO(), instance)
			if err != nil {
				return ctrl.Result{}, err
			}
			r.Log.Info(fmt.Sprintf("Finalizer %s added to CR %s", finalizerName, instance.Name))
		}

	} else {
		// 1. check if finalizer is there
		// Reconcile if finalizer got already removed
		if !controllerutil.ContainsFinalizer(instance, finalizerName) {
			return ctrl.Result{}, nil
		}

		// 2. delete the database
		r.Log.Info(fmt.Sprintf("CR %s delete, running DB delete job", instance.Name))
		job := mariadb.DeleteDbDatabaseJob(instance, db.Name, db.Spec.Secret, db.Spec.ContainerImage)

		requeue, err := util.EnsureJob(job, r.Client, r.Log)
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			r.Log.Info("Waiting on DB delete")
			return ctrl.Result{RequeueAfter: time.Second * 5}, err
		}

		// delete the job
		requeue, err = util.DeleteJob(job, r.Kclient, r.Log)
		if err != nil {
			return ctrl.Result{}, err
		}

		// 3. as last step remove the finalizer on the operator CR to finish delete
		controllerutil.RemoveFinalizer(instance, finalizerName)
		err = r.Client.Update(context.TODO(), instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.Log.Info(fmt.Sprintf("CR %s deleted", instance.Name))
		return ctrl.Result{}, nil
	}

	if db.Status.DbInitHash == "" {
		r.Log.Info("DB initialization not complete. Requeue...")
		return ctrl.Result{RequeueAfter: time.Second * 10}, err
	}

	// Define a new Job object (hostname, password, containerImage)
	job := mariadb.DbDatabaseJob(instance, db.Name, db.Spec.Secret, db.Spec.ContainerImage)

	requeue := true
	if instance.Status.Completed {
		requeue, err = util.EnsureJob(job, r.Client, r.Log)
		r.Log.Info("Creating database...")
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			r.Log.Info("Waiting on database creation job...")
			return ctrl.Result{RequeueAfter: time.Second * 5}, err
		}
	}
	// database creation finished... okay to set to completed
	if err := r.setCompleted(instance); err != nil {
		return ctrl.Result{}, err
	}
	// delete the job
	requeue, err = util.DeleteJob(job, r.Kclient, r.Log)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *MariaDBDatabaseReconciler) setCompleted(db *databasev1beta1.MariaDBDatabase) error {

	if !db.Status.Completed {
		db.Status.Completed = true
		if err := r.Client.Status().Update(context.TODO(), db); err != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager -
func (r *MariaDBDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1beta1.MariaDBDatabase{}).
		Complete(r)
}
