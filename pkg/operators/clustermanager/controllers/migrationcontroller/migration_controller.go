package migrationcontroller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehelper"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	operatorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	migrationv1alpha1 "sigs.k8s.io/kube-storage-version-migrator/pkg/apis/migration/v1alpha1"
	migrationv1alpha1client "sigs.k8s.io/kube-storage-version-migrator/pkg/clients/clientset/typed/migration/v1alpha1"

	operatorinformer "open-cluster-management.io/api/client/operator/informers/externalversions/operator/v1"
	operatorlister "open-cluster-management.io/api/client/operator/listers/operator/v1"
	"open-cluster-management.io/registration-operator/manifests"
)

var (
	genericScheme = runtime.NewScheme()
	genericCodecs = serializer.NewCodecFactory(genericScheme)
	genericCodec  = genericCodecs.UniversalDeserializer()

	_ = migrationv1alpha1.AddToScheme(genericScheme)

	migrationRequestFiles = []string{
		"cluster-manager/cluster-manager-managedclustersets-migration.yaml",
		"cluster-manager/cluster-manager-managedclustersetbindings-migration.yaml",
	}
)

const (
	clusterManagerApplied   = "Applied"
	migrationRequestCRDName = "storageversionmigrations.migration.k8s.io"
)

type crdMigrationController struct {
	clusterManagerLister operatorlister.ClusterManagerLister
	apiExtensionClient   apiextensionsclient.Interface
	migrationClient      migrationv1alpha1client.StorageVersionMigrationsGetter
}

// NewClusterManagerController construct cluster manager hub controller
func NewCRDMigrationController(
	apiExtensionClient apiextensionsclient.Interface,
	migrationClient migrationv1alpha1client.StorageVersionMigrationsGetter,
	clusterManagerInformer operatorinformer.ClusterManagerInformer,
	recorder events.Recorder) factory.Controller {
	controller := &crdMigrationController{
		apiExtensionClient:   apiExtensionClient,
		migrationClient:      migrationClient,
		clusterManagerLister: clusterManagerInformer.Lister(),
	}

	return factory.New().WithSync(controller.sync).
		WithInformersQueueKeyFunc(func(obj runtime.Object) string {
			accessor, _ := meta.Accessor(obj)
			return accessor.GetName()
		}, clusterManagerInformer.Informer()).
		ToController("CRDMigrationController", recorder)
}

func (c *crdMigrationController) sync(ctx context.Context, controllerContext factory.SyncContext) error {
	clusterManagerName := controllerContext.QueueKey()
	klog.V(4).Infof("Reconciling ClusterManager %q", clusterManagerName)

	clusterManager, err := c.clusterManagerLister.Get(clusterManagerName)
	if errors.IsNotFound(err) {
		// ClusterManager not found, could have been deleted, do nothing.
		return nil
	}
	if err != nil {
		return err
	}

	// apply storage version migrations if it is supported
	supported, err := c.supportStorageVersionMigration(ctx)
	if err != nil {
		return err
	}
	if !supported {
		return nil
	}

	// ClusterManager is deleting, we remove its related resources on hub
	if !clusterManager.DeletionTimestamp.IsZero() {
		return c.removeStorageVersionMigrations(ctx)
	}

	// do not apply storage version migrations until other resources are applied
	if applied := meta.IsStatusConditionTrue(clusterManager.Status.Conditions, clusterManagerApplied); !applied {
		controllerContext.Queue().AddAfter(clusterManagerName, 5*time.Second)
		return nil
	}

	return c.applyStorageVersionMigrations(ctx, controllerContext.Recorder())
}

// supportStorageVersionMigration returns ture if StorageVersionMigration CRD exists; otherwise returns false.
func (c *crdMigrationController) supportStorageVersionMigration(ctx context.Context) (bool, error) {
	_, err := c.apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, migrationRequestCRDName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *crdMigrationController) removeStorageVersionMigrations(
	ctx context.Context) error {
	// Reomve storage version migrations
	for _, file := range migrationRequestFiles {
		err := removeStorageVersionMigration(
			ctx,
			c.migrationClient,
			func(name string) ([]byte, error) {
				template, err := manifests.ClusterManagerManifestFiles.ReadFile(name)
				if err != nil {
					return nil, err
				}
				return assets.MustCreateAssetFromTemplate(name, template, struct{}{}).Data, nil
			},
			file,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *crdMigrationController) applyStorageVersionMigrations(ctx context.Context, recorder events.Recorder) error {
	errs := []error{}
	for _, file := range migrationRequestFiles {
		_, _, err := applyStorageVersionMigration(
			c.migrationClient,
			func(name string) ([]byte, error) {
				template, err := manifests.ClusterManagerManifestFiles.ReadFile(name)
				if err != nil {
					return nil, err
				}
				return assets.MustCreateAssetFromTemplate(name, template, struct{}{}).Data, nil
			},
			recorder,
			file)
		if err != nil {
			errs = append(errs, err)
		}
	}

	return operatorhelpers.NewMultiLineAggregate(errs)
}

func removeStorageVersionMigration(
	ctx context.Context,
	migrationClient migrationv1alpha1client.StorageVersionMigrationsGetter,
	manifests resourceapply.AssetFunc,
	file string) error {
	objectRaw, err := manifests(file)
	if err != nil {
		return err
	}
	object, _, err := genericCodec.Decode(objectRaw, nil, nil)
	if err != nil {
		return err
	}

	required, ok := object.(*migrationv1alpha1.StorageVersionMigration)
	if !ok {
		return fmt.Errorf("invalid StorageVersionMigration in file %q: %v", file, object)
	}

	err = migrationClient.StorageVersionMigrations().Delete(ctx, required.Name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func applyStorageVersionMigration(
	client migrationv1alpha1client.StorageVersionMigrationsGetter,
	manifests resourceapply.AssetFunc,
	recorder events.Recorder,
	file string) (*migrationv1alpha1.StorageVersionMigration, bool, error) {
	objBytes, err := manifests(file)
	if err != nil {
		return nil, false, fmt.Errorf("missing %q: %v", file, err)
	}
	requiredObj, _, err := genericCodec.Decode(objBytes, nil, nil)
	if err != nil {
		return nil, false, fmt.Errorf("cannot decode %q: %v", file, err)
	}

	required, ok := requiredObj.(*migrationv1alpha1.StorageVersionMigration)
	if !ok {
		return nil, false, fmt.Errorf("invalid StorageVersionMigration in file %q: %v", file, requiredObj)
	}

	existing, err := client.StorageVersionMigrations().Get(context.TODO(), required.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		actual, err := client.StorageVersionMigrations().Create(context.TODO(), required, metav1.CreateOptions{})
		if err != nil {
			recorder.Warningf("StorageVersionMigrationCreateFailed", "Failed to create %s: %v", resourcehelper.FormatResourceForCLIWithNamespace(required), err)
			return actual, true, err
		}

		recorder.Eventf("StorageVersionMigrationCreated", "Created %s because it was missing", resourcehelper.FormatResourceForCLIWithNamespace(actual))
		return actual, true, err
	}
	if err != nil {
		return nil, false, err
	}

	modified := resourcemerge.BoolPtr(false)
	existingCopy := existing.DeepCopy()
	resourcemerge.EnsureObjectMeta(modified, &existingCopy.ObjectMeta, required.ObjectMeta)
	if !equality.Semantic.DeepEqual(existingCopy.Spec, required.Spec) {
		*modified = true
		existing.Spec = required.Spec
	}
	if !*modified {
		return existing, false, nil
	}

	actual, err := client.StorageVersionMigrations().Update(context.TODO(), existingCopy, metav1.UpdateOptions{})
	if err != nil {
		recorder.Warningf("StorageVersionMigrationUpdateFailed", "Failed to update %s: %v", resourcehelper.FormatResourceForCLIWithNamespace(existingCopy), err)
		return actual, true, err
	}
	recorder.Eventf("StorageVersionMigrationUpdated", "Updated %s because it changed", resourcehelper.FormatResourceForCLIWithNamespace(actual))
	return actual, true, nil
}
