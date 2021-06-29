package reconciler

import (
	"fmt"

	"github.com/go-logr/logr"
	sgv1alpha1 "github.com/vmware-tanzu/carvel-secretgen-controller/pkg/apis/secretgen/v1alpha1"
	sgclient "github.com/vmware-tanzu/carvel-secretgen-controller/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type SecretImportReconciler struct {
	sgClient   sgclient.Interface
	coreClient kubernetes.Interface
	log        logr.Logger
}

var _ reconcile.Reconciler = &SecretImportReconciler{}

func NewSecretImportReconciler(sgClient sgclient.Interface,
	coreClient kubernetes.Interface, log logr.Logger) *SecretImportReconciler {
	return &SecretImportReconciler{sgClient, coreClient, log}
}

func (r *SecretImportReconciler) AttachWatches(controller controller.Controller) error {
	var errs []error

	// Watch secrets and enqueue for same named SecretImport
	// to make sure imported secret is up-to-date
	errs = append(errs, controller.Watch(
		&source.Kind{Type: &corev1.Secret{}},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.mapSecretToRequest)},
	))

	// Watch SecretExport and enqueue for related SecretImport
	// based on export namespace configuration
	errs = append(errs, controller.Watch(
		&source.Kind{Type: &sgv1alpha1.SecretExport{}},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.mapExportsToRequests)},
	))

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *SecretImportReconciler) mapSecretToRequest(a handler.MapObject) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{
			Name:      a.Meta.GetName(),
			Namespace: a.Meta.GetNamespace(),
		}},
	}
}

func (r *SecretImportReconciler) mapExportsToRequests(a handler.MapObject) []reconcile.Request {
	var export sgv1alpha1.SecretExport
	var result []reconcile.Request

	err := scheme.Scheme.Convert(a.Object, &export, nil)
	if err != nil {
		return nil
	}

	// Skip exports that are not fully reconciled
	// New events will be emitted when reconciliation finishes
	if !(&Status{s: export.Status.GenericStatus}).IsReconcileSucceeded() {
		return nil
	}

	for _, ns := range export.StaticToNamespaces() {
		result = append(result, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      a.Meta.GetName(),
				Namespace: ns,
			},
		})
	}

	return result
}

func (r *SecretImportReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	secretdRequest, err := r.sgClient.SecretgenV1alpha1().SecretImports(
		request.Namespace).Get(request.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Do not requeue as there is nothing to do when request is deleted
			return reconcile.Result{}, nil
		}
		// Requeue to try to fetch request again
		return reconcile.Result{Requeue: true}, err
	}

	if secretdRequest.DeletionTimestamp != nil {
		// Do not requeue as there is nothing to do
		// Associated secret has owned ref so it's going to be deleted
		return reconcile.Result{}, nil
	}

	status := &Status{
		secretdRequest.Status.GenericStatus,
		func(st sgv1alpha1.GenericStatus) { secretdRequest.Status.GenericStatus = st },
	}

	status.SetReconciling(secretdRequest.ObjectMeta)
	defer r.updateStatus(secretdRequest)

	return status.WithReconcileCompleted(r.reconcile(secretdRequest))
}

func (r *SecretImportReconciler) reconcile(
	secretImport *sgv1alpha1.SecretImport) (reconcile.Result, error) {

	err := secretImport.Validate()
	if err != nil {
		// Do not requeue as there is nothing this controller can do until secret request is fixed
		return reconcile.Result{}, err
	}

	notOfferedMsg := "Export was not offered (even though requested)"
	notAllowedMsg := "Export was not allowed (even though requested)"

	secretExport, err := r.sgClient.SecretgenV1alpha1().SecretExports(
		secretImport.Spec.FromNamespace).Get(secretImport.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// TODO Should we actually delete offered secret that we previously created?
			err := r.deleteAssociatedSecret(secretImport)
			if err != nil {
				// Requeue to try to delete a bit later
				return reconcile.Result{Requeue: true}, fmt.Errorf("%s: %s", notOfferedMsg, err)
			}
			// Do not requeue since export is not offered
			return reconcile.Result{}, fmt.Errorf("%s", notOfferedMsg)
		}
		// Requeue to try to find secret export
		return reconcile.Result{Requeue: true}, fmt.Errorf("Finding export: %s", err)
	}

	if !r.isExportAllowed(secretExport, secretImport) {
		err := r.deleteAssociatedSecret(secretImport)
		if err != nil {
			// Requeue to try to delete a bit later
			return reconcile.Result{Requeue: true}, fmt.Errorf("%s: %s", notAllowedMsg, err)
		}
		// Do not requeue since export is not allowed
		return reconcile.Result{}, fmt.Errorf("%s", notAllowedMsg)
	}

	return r.copyAssociatedSecret(secretImport)
}

func (r *SecretImportReconciler) isExportAllowed(
	export *sgv1alpha1.SecretExport, secretImport *sgv1alpha1.SecretImport) bool {

	if export.Spec.ToNamespace == secretImport.Namespace {
		return true
	}
	for _, exportNs := range export.Spec.ToNamespaces {
		if exportNs == secretImport.Namespace {
			return true
		}
	}
	return false
}

func (r *SecretImportReconciler) copyAssociatedSecret(
	secretImport *sgv1alpha1.SecretImport) (reconcile.Result, error) {

	srcSecret, err := r.coreClient.CoreV1().Secrets(
		secretImport.Spec.FromNamespace).Get(secretImport.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// TODO Should we actually delete offered secret that we previously created?
			err := r.deleteAssociatedSecret(secretImport)
			if err != nil {
				// Requeue to try to delete a bit later
				return reconcile.Result{Requeue: true}, err
			}
			// Do not requeue since there is nothing this controller can do to fix that
			return reconcile.Result{}, nil
		}
		// Requeue to try to fetch a bit later
		return reconcile.Result{Requeue: true}, fmt.Errorf("Getting exported secret: %s", err)
	}

	secret := NewSecret(secretImport, nil)
	secret.ApplySecret(srcSecret)

	_, err = r.coreClient.CoreV1().Secrets(secretImport.Namespace).Create(secret.AsSecret())
	switch {
	case err == nil:
		// Do not requeue since we copied secret successfully
		return reconcile.Result{}, nil

	case errors.IsAlreadyExists(err):
		existingSecret, err := r.coreClient.CoreV1().Secrets(secretImport.Namespace).Get(
			secretImport.Name, metav1.GetOptions{})
		if err != nil {
			// Requeue to try to fetch a bit later
			return reconcile.Result{Requeue: true}, fmt.Errorf("Getting imported secret: %s", err)
		}

		secret.AssociteExistingSecret(existingSecret)

		_, err = r.coreClient.CoreV1().Secrets(secretImport.Namespace).Update(secret.AsSecret())
		if err != nil {
			// Requeue to try to update a bit later
			return reconcile.Result{Requeue: true}, fmt.Errorf("Updating imported secret: %s", err)
		}

		// Do not requeue since we copied secret successfully
		return reconcile.Result{}, nil

	default:
		// Requeue to try to create a bit later
		return reconcile.Result{Requeue: true}, fmt.Errorf("Creating imported secret: %s", err)
	}
}

func (r *SecretImportReconciler) deleteAssociatedSecret(
	secretImport *sgv1alpha1.SecretImport) error {

	err := r.coreClient.CoreV1().Secrets(secretImport.Namespace).Delete(
		secretImport.Name, &metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("Deleting associated secret: %s", err)
	}
	return nil
}

func (r *SecretImportReconciler) updateStatus(
	secretImport *sgv1alpha1.SecretImport) error {

	existingSecretImport, err := r.sgClient.SecretgenV1alpha1().SecretImports(
		secretImport.Namespace).Get(secretImport.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Fetching secret export: %s", err)
	}

	existingSecretImport.Status = secretImport.Status

	_, err = r.sgClient.SecretgenV1alpha1().SecretImports(
		secretImport.Namespace).UpdateStatus(secretImport)
	if err != nil {
		return fmt.Errorf("Updating secret export status: %s", err)
	}

	return nil
}
