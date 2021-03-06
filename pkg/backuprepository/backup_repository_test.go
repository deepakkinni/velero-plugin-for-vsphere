package backuprepository

import (
	"context"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/constants"
	veleroplugintest "github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/test"
	"github.com/vmware-tanzu/velero/pkg/generated/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	backupdriverv1 "github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/apis/backupdriver/v1alpha1"
	backupdriverTypedV1 "github.com/vmware-tanzu/velero-plugin-for-vsphere/pkg/generated/clientset/versioned/typed/backupdriver/v1alpha1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func TestClaimBackupRepository(t *testing.T) {
	path := os.Getenv("KUBECONFIG")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skipf("The KubeConfig file, %v, is not exist", path)
	}

	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("Failed to build k8s config from kubeconfig file: %+v ", err)
	}

	// Setup Logger
	logger := logrus.New()
	formatter := new(logrus.TextFormatter)
	formatter.TimestampFormat = time.RFC3339Nano
	formatter.FullTimestamp = true
	logger.SetFormatter(formatter)
	logger.SetLevel(logrus.DebugLevel)

	// using velero ns for testing.
	veleroNs := "velero"

	backupdriverClient, err := backupdriverTypedV1.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to retrieve backupdriverClient from config: %v", config)
	}
	repositoryParameters := make(map[string]string)
	ctx := context.Background()
	testDoneStatus := make(chan bool)

	// The following anon function triggers ClaimBackupRepository and waits for the BR
	go func() {
		backupRepositoryName, err := ClaimBackupRepository(ctx, constants.S3RepositoryDriver, repositoryParameters,
			[]string{"test"}, veleroNs, backupdriverClient, logger)
		if err != nil {
			t.Fatalf("Failed to retrieve the BackupRepository name.")
		}
		logger.Infof("Successfully retrieved the BackupRepository name: %s", backupRepositoryName)
		// Trigger a test complete.
		testDoneStatus <- true
	}()

	// Wait for the BRC to be created and create corresponding BR.
	watchlist := cache.NewListWatchFromClient(backupdriverClient.RESTClient(),
		"backuprepositoryclaims", veleroNs, fields.Everything())
	_, controller := cache.NewInformer(
		watchlist,
		&backupdriverv1.BackupRepositoryClaim{},
		time.Second*0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				backupRepoClaim := obj.(*backupdriverv1.BackupRepositoryClaim)
				logger.Infof("Backup Repository Claim added: %s", backupRepoClaim.Name)
				err = handleNewBackupRepositoryClaim(ctx, backupRepoClaim, veleroNs, backupdriverClient, logger)
				if err != nil {
					t.Fatalf("The test failed while processing new BRC.")
				}
				logger.Infof("Successfully created BR and patched BRC to point to BR.")
			},
		})
	stop := make(chan struct{})
	go controller.Run(stop)
	select {
	case <-ctx.Done():
		stop <- struct{}{}
	case <-testDoneStatus:
		logger.Infof("Test completed successfully")
	}
}

func TestBackupRepositoryCreationFromBSL(t *testing.T) {
	path := os.Getenv("KUBECONFIG")
	veleroNs := os.Getenv("VELERO_NAMESPACE")
	region := os.Getenv("REGION")
	bucket := os.Getenv("BUCKET")
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	ctx := context.Background()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skipf("The KubeConfig file, %v, is not exist", path)
	}

	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("Failed to build k8s config from kubeconfig file: %+v ", err)
	}

	// Setup Logger
	logger := logrus.New()
	formatter := new(logrus.TextFormatter)
	formatter.TimestampFormat = time.RFC3339Nano
	formatter.FullTimestamp = true
	logger.SetFormatter(formatter)
	logger.SetLevel(logrus.DebugLevel)

	veleroClient, err := versioned.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to retrieve veleroClient")
	}

	backupdriverClient, err := backupdriverTypedV1.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to retrieve backupdriverClient from config: %v", config)
	}

	backupStorageLocationList, err := veleroClient.VeleroV1().BackupStorageLocations(veleroNs).List(context.TODO(), metav1.ListOptions{})
	if err != nil || len(backupStorageLocationList.Items) <= 0 {
		t.Fatalf("RetrieveVSLFromVeleroBSLs: Failed to list Velero default backup storage location")
	}
	repositoryParameters := make(map[string]string)
	repositoryParameters["region"] = region
	repositoryParameters["bucket"] = bucket
	repositoryParameters[constants.AWS_ACCESS_KEY_ID] = accessKeyID
	repositoryParameters[constants.AWS_SECRET_ACCESS_KEY] = secretAccessKey

	logger.Infof("Repository Parameters: %v", repositoryParameters)
	backupRepositoryName, err := ClaimBackupRepository(ctx, constants.S3RepositoryDriver, repositoryParameters,
		[]string{"test"}, veleroNs, backupdriverClient, logger)
	if err != nil {
		t.Fatalf("Failed to retrieve the BackupRepository name.")
	}
	logger.Infof("Successfully retrieved the BackupRepository name: %s", backupRepositoryName)
	// TODO: Manually verify in supervisor cluster for the corresponding BR.
}

// This function creates a new BR in response to the BRC.
// Patches the BRC with the BR
func handleNewBackupRepositoryClaim(ctx context.Context,
	backupRepositoryClaim *backupdriverv1.BackupRepositoryClaim,
	ns string,
	backupdriverClient *backupdriverTypedV1.BackupdriverV1alpha1Client,
	logger logrus.FieldLogger) error {
	backupRepository, err := CreateBackupRepository(ctx, backupRepositoryClaim, "", backupdriverClient, logger)
	if err != nil {
		logger.Errorf("Failed to create the BackupRepository")
		return err
	}
	err = PatchBackupRepositoryClaim(backupRepositoryClaim, backupRepository.Name, ns, backupdriverClient)
	if err != nil {
		logger.Errorf("Failed to patch the BRC with the newly created BR")
	}
	return nil
}

func TestCreateRepositoryFromBackupRepository(t *testing.T) {
	map1 := make(map[string]string)
	map2 := make(map[string]string)
	map2["region"] = "us-west-1"
	tests := []struct {
		name             string
		key              string
		backupRepository *backupdriverv1.BackupRepository
		expectedErr      error
	}{
		{
			name: "Unsupported backup driver type returns error",
			key:  "backupdriver/backuprepository-1",
			backupRepository: &backupdriverv1.BackupRepository{
				TypeMeta: metav1.TypeMeta{
					APIVersion: backupdriverv1.SchemeGroupVersion.String(),
					Kind:       "BackupRepository",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "default",
				},
				RepositoryDriver: "unsupported-driver",
			},
			expectedErr: errors.New("Unsupported backuprepository driver type: unsupported-driver. Only support s3repository.astrolabe.vmware-tanzu.com."),
		},
		{
			name: "Repository parameter missing region should return error",
			key:  "miss-region",
			backupRepository: &backupdriverv1.BackupRepository{
				TypeMeta: metav1.TypeMeta{
					APIVersion: backupdriverv1.SchemeGroupVersion.String(),
					Kind:       "BackupRepository",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "default",
				},
				RepositoryDriver:     constants.S3RepositoryDriver,
				RepositoryParameters: map1,
			},
			expectedErr: errors.New("Missing region param, cannot initialize S3 PETM"),
		},
		{
			name: "Repository parameter missing bucket should return error",
			key:  "miss-bucket",
			backupRepository: &backupdriverv1.BackupRepository{
				TypeMeta: metav1.TypeMeta{
					APIVersion: backupdriverv1.SchemeGroupVersion.String(),
					Kind:       "BackupRepository",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "default",
				},
				RepositoryDriver:     constants.S3RepositoryDriver,
				RepositoryParameters: map2,
			},
			expectedErr: errors.New("Missing bucket param, cannot initialize S3 PETM"),
		},
	}
	for _, test := range tests {
		var (
			logger = veleroplugintest.NewLogger()
		)

		t.Run(test.name, func(t *testing.T) {
			_, err := GetRepositoryFromBackupRepository(test.backupRepository, logger)
			assert.Equal(t, test.expectedErr.Error(), err.Error())
		})
	}
}
