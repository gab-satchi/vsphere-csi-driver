/*
Copyright 2020 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	vim25types "github.com/vmware/govmomi/vim25/types"
	apps "k8s.io/api/apps/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/test/e2e/framework"
	fpod "k8s.io/kubernetes/test/e2e/framework/pod"
	fpv "k8s.io/kubernetes/test/e2e/framework/pv"
	fssh "k8s.io/kubernetes/test/e2e/framework/ssh"
	"k8s.io/kubernetes/test/e2e/manifest"

	cnsoperatorv1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/cnsoperator"
	cnsnodevmattachmentv1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/cnsoperator/cnsnodevmattachment/v1alpha1"
	"sigs.k8s.io/vsphere-csi-driver/pkg/apis/cnsoperator/cnsregistervolume/v1alpha1"
	cnsregistervolumev1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/cnsoperator/cnsregistervolume/v1alpha1"
	cnsvolumemetadatav1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/cnsoperator/cnsvolumemetadata/v1alpha1"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	k8s "sigs.k8s.io/vsphere-csi-driver/pkg/kubernetes"
)

var (
	svcClient    clientset.Interface
	svcNamespace string
)

// getVSphereStorageClassSpec returns Storage Class Spec with supplied storage class parameters
func getVSphereStorageClassSpec(scName string, scParameters map[string]string, allowedTopologies []v1.TopologySelectorLabelRequirement, scReclaimPolicy v1.PersistentVolumeReclaimPolicy, bindingMode storagev1.VolumeBindingMode, allowVolumeExpansion bool) *storagev1.StorageClass {
	if bindingMode == "" {
		bindingMode = storagev1.VolumeBindingImmediate
	}
	var sc = &storagev1.StorageClass{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sc-",
		},
		Provisioner:          e2evSphereCSIBlockDriverName,
		VolumeBindingMode:    &bindingMode,
		AllowVolumeExpansion: &allowVolumeExpansion,
	}
	// If scName is specified, use that name, else auto-generate storage class name
	if scName != "" {
		sc.ObjectMeta.Name = scName
	}
	if scParameters != nil {
		sc.Parameters = scParameters
	}
	if allowedTopologies != nil {
		sc.AllowedTopologies = []v1.TopologySelectorTerm{
			{
				MatchLabelExpressions: allowedTopologies,
			},
		}
	}
	if scReclaimPolicy != "" {
		sc.ReclaimPolicy = &scReclaimPolicy
	}

	return sc
}

// getPvFromClaim returns PersistentVolume for requested claim
func getPvFromClaim(client clientset.Interface, namespace string, claimName string) *v1.PersistentVolume {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pvclaim, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, pvclaim.Spec.VolumeName, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return pv
}

// getNodeUUID returns Node VM UUID for requested node
func getNodeUUID(client clientset.Interface, nodeName string) string {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	vmUUID := strings.TrimPrefix(node.Spec.ProviderID, providerPrefix)
	gomega.Expect(vmUUID).NotTo(gomega.BeEmpty())
	ginkgo.By(fmt.Sprintf("VM UUID is: %s for node: %s", vmUUID, nodeName))
	return vmUUID
}

// getVMUUIDFromNodeName returns the vmUUID for a given node vm and datacenter
func getVMUUIDFromNodeName(nodeName string) (string, error) {
	var datacenters []string
	finder := find.NewFinder(e2eVSphere.Client.Client, false)
	cfg, err := getConfig()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	dcList := strings.Split(cfg.Global.Datacenters, ",")
	for _, dc := range dcList {
		dcName := strings.TrimSpace(dc)
		if dcName != "" {
			datacenters = append(datacenters, dcName)
		}
	}
	var vm *object.VirtualMachine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, dc := range datacenters {
		dataCenter, err := finder.Datacenter(ctx, dc)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		finder := find.NewFinder(dataCenter.Client(), false)
		finder.SetDatacenter(dataCenter)
		vm, err = finder.VirtualMachine(ctx, nodeName)
		if err != nil {
			continue
		}
		vmUUID := vm.UUID(ctx)
		gomega.Expect(vmUUID).NotTo(gomega.BeEmpty())
		ginkgo.By(fmt.Sprintf("VM UUID is: %s for node: %s", vmUUID, nodeName))
		return vmUUID, nil
	}
	return "", err
}

// verifyVolumeMetadataInCNS verifies container volume metadata is matching the one is CNS cache
func verifyVolumeMetadataInCNS(vs *vSphere, volumeID string, PersistentVolumeClaimName string, PersistentVolumeName string,
	PodName string, Labels ...types.KeyValue) error {
	queryResult, err := vs.queryCNSVolumeWithResult(volumeID)
	if err != nil {
		return err
	}
	gomega.Expect(queryResult.Volumes).ShouldNot(gomega.BeEmpty())
	if len(queryResult.Volumes) != 1 || queryResult.Volumes[0].VolumeId.Id != volumeID {
		return fmt.Errorf("failed to query cns volume %s", volumeID)
	}
	for _, metadata := range queryResult.Volumes[0].Metadata.EntityMetadata {
		kubernetesMetadata := metadata.(*cnstypes.CnsKubernetesEntityMetadata)
		if kubernetesMetadata.EntityType == "POD" && kubernetesMetadata.EntityName != PodName {
			return fmt.Errorf("entity POD with name %s not found for volume %s", PodName, volumeID)
		} else if kubernetesMetadata.EntityType == "PERSISTENT_VOLUME" && kubernetesMetadata.EntityName != PersistentVolumeName {
			return fmt.Errorf("entity PV with name %s not found for volume %s", PersistentVolumeName, volumeID)
		} else if kubernetesMetadata.EntityType == "PERSISTENT_VOLUME_CLAIM" && kubernetesMetadata.EntityName != PersistentVolumeClaimName {
			return fmt.Errorf("entity PVC with name %s not found for volume %s", PersistentVolumeClaimName, volumeID)
		}
	}
	labelMap := make(map[string]string)
	for _, e := range queryResult.Volumes[0].Metadata.EntityMetadata {
		if e == nil {
			continue
		}
		if e.GetCnsEntityMetadata().Labels == nil {
			continue
		}
		for _, al := range e.GetCnsEntityMetadata().Labels {
			// these are the actual labels in the provisioned PV. populate them in the label map
			labelMap[al.Key] = al.Value
		}
		for _, el := range Labels {
			// Traverse through the slice of expected labels and see if all of them are present in the label map
			if val, ok := labelMap[el.Key]; ok {
				gomega.Expect(el.Value == val).To(gomega.BeTrue(),
					fmt.Sprintf("Actual label Value of the statically provisioned PV is %s but expected is %s", val, el.Value))
			} else {
				return fmt.Errorf("label(%s:%s) is expected in the provisioned PV but its not found", el.Key, el.Value)
			}
		}
	}
	ginkgo.By(fmt.Sprintf("successfully verified metadata of the volume %q", volumeID))
	return nil
}

// getVirtualDeviceByDiskID gets the virtual device by diskID
func getVirtualDeviceByDiskID(ctx context.Context, vm *object.VirtualMachine, diskID string) (vim25types.BaseVirtualDevice, error) {
	vmname, err := vm.Common.ObjectName(ctx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	vmDevices, err := vm.Device(ctx)
	if err != nil {
		framework.Logf("failed to get the devices for VM: %q. err: %+v", vmname, err)
		return nil, err
	}
	for _, device := range vmDevices {
		if vmDevices.TypeName(device) == "VirtualDisk" {
			if virtualDisk, ok := device.(*vim25types.VirtualDisk); ok {
				if virtualDisk.VDiskId != nil && virtualDisk.VDiskId.Id == diskID {
					framework.Logf("Found FCDID %q attached to VM %q", diskID, vmname)
					return device, nil
				}
			}
		}
	}
	framework.Logf("failed to find FCDID %q attached to VM %q", diskID, vmname)
	return nil, nil
}

// getPersistentVolumeClaimSpecWithStorageClass return the PersistentVolumeClaim spec with specified storage class
func getPersistentVolumeClaimSpecWithStorageClass(namespace string, ds string, storageclass *storagev1.StorageClass, pvclaimlabels map[string]string, accessMode v1.PersistentVolumeAccessMode) *v1.PersistentVolumeClaim {
	disksize := diskSize
	if ds != "" {
		disksize = ds
	}
	if accessMode == "" {
		// if accessMode is not specified, set the default accessMode
		accessMode = v1.ReadWriteOnce
	}
	claim := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-",
			Namespace:    namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				accessMode,
			},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse(disksize),
				},
			},
			StorageClassName: &(storageclass.Name),
		},
	}

	if pvclaimlabels != nil {
		claim.Labels = pvclaimlabels
	}

	return claim
}

// createPVCAndStorageClass helps creates a storage class with specified name, storageclass parameters and PVC using storage class
func createPVCAndStorageClass(client clientset.Interface, pvcnamespace string, pvclaimlabels map[string]string, scParameters map[string]string, ds string,
	allowedTopologies []v1.TopologySelectorLabelRequirement, bindingMode storagev1.VolumeBindingMode, allowVolumeExpansion bool, accessMode v1.PersistentVolumeAccessMode, names ...string) (*storagev1.StorageClass, *v1.PersistentVolumeClaim, error) {
	scName := ""
	if len(names) > 0 {
		scName = names[0]
	}
	storageclass, err := createStorageClass(client, scParameters, allowedTopologies, "", bindingMode, allowVolumeExpansion, scName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	pvclaim, err := createPVC(client, pvcnamespace, pvclaimlabels, ds, storageclass, accessMode)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	return storageclass, pvclaim, err
}

// createStorageClass helps creates a storage class with specified name, storageclass parameters
func createStorageClass(client clientset.Interface, scParameters map[string]string, allowedTopologies []v1.TopologySelectorLabelRequirement,
	scReclaimPolicy v1.PersistentVolumeReclaimPolicy, bindingMode storagev1.VolumeBindingMode, allowVolumeExpansion bool, scName string) (*storagev1.StorageClass, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ginkgo.By(fmt.Sprintf("Creating StorageClass [%q] With scParameters: %+v and allowedTopologies: %+v and ReclaimPolicy: %+v and allowVolumeExpansion: %t", scName, scParameters, allowedTopologies, scReclaimPolicy, allowVolumeExpansion))
	storageclass, err := client.StorageV1().StorageClasses().Create(ctx, getVSphereStorageClassSpec(scName, scParameters, allowedTopologies, scReclaimPolicy, bindingMode, allowVolumeExpansion), metav1.CreateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("Failed to create storage class with err: %v", err))
	return storageclass, err
}

// createPVC helps creates pvc with given namespace and labels using given storage class
func createPVC(client clientset.Interface, pvcnamespace string, pvclaimlabels map[string]string, ds string, storageclass *storagev1.StorageClass, accessMode v1.PersistentVolumeAccessMode) (*v1.PersistentVolumeClaim, error) {
	pvcspec := getPersistentVolumeClaimSpecWithStorageClass(pvcnamespace, ds, storageclass, pvclaimlabels, accessMode)
	ginkgo.By(fmt.Sprintf("Creating PVC using the Storage Class %s with disk size %s and labels: %+v accessMode: %+v", storageclass.Name, ds, pvclaimlabels, accessMode))
	pvclaim, err := fpv.CreatePVC(client, pvcnamespace, pvcspec)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("Failed to create pvc with err: %v", err))
	return pvclaim, err
}

// createStatefulSetWithOneReplica helps create a stateful set with one replica
func createStatefulSetWithOneReplica(client clientset.Interface, manifestPath string, namespace string) (*appsv1.StatefulSet, *v1.Service) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mkpath := func(file string) string {
		return filepath.Join(manifestPath, file)
	}
	statefulSet, err := manifest.StatefulSetFromManifest(mkpath("statefulset.yaml"), namespace)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	service, err := manifest.SvcFromManifest(mkpath("service.yaml"))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	service, err = client.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	*statefulSet.Spec.Replicas = 1
	_, err = client.AppsV1().StatefulSets(namespace).Create(ctx, statefulSet, metav1.CreateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return statefulSet, service
}

// updateDeploymentReplica helps to update the replica for a deployment
func updateDeploymentReplica(client clientset.Interface, count int32, name string, namespace string) *appsv1.Deployment {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	*deployment.Spec.Replicas = count
	deployment, err = client.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.By("Waiting for update operation on deployment to take effect")
	time.Sleep(1 * time.Minute)
	return deployment
}

// bringDownCsiContorller helps to bring the csi controller pod down
// Its taks svc/gc client as input
func bringDownCsiContorller(Client clientset.Interface) {
	updateDeploymentReplica(Client, 0, vsphereCSIcontroller, vsphereSystemNamespace)
	ginkgo.By("Controller is down")
}

// bringDownTKGController helps to bring the TKG control manager pod down
// Its taks svc client as input
func bringDownTKGController(Client clientset.Interface) {
	updateDeploymentReplica(Client, 0, vsphereControllerManager, vsphereTKGSystemNamespace)
	ginkgo.By("TKGControllManager replica is set to 0")
}

// bringUpCsiContorller helps to bring the csi controller pod down
// Its taks svc/gc client as input
func bringUpCsiContorller(gcClient clientset.Interface) {
	updateDeploymentReplica(gcClient, 1, vsphereCSIcontroller, vsphereSystemNamespace)
	ginkgo.By("Controller is up")

}

// bringUpTKGController helps to bring the TKG control manager pod up
// Its taks svc client as input
func bringUpTKGController(Client clientset.Interface) {
	updateDeploymentReplica(Client, 1, vsphereControllerManager, vsphereTKGSystemNamespace)
	ginkgo.By("TKGControllManager is up")
}

func getSvcClientAndNamespace() (clientset.Interface, string) {
	var err error
	if svcClient == nil {
		if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
			svcClient, err = k8s.CreateKubernetesClientFromConfig(k8senv)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		svcNamespace = GetAndExpectStringEnvVar(envSupervisorClusterNamespace)
	}
	return svcClient, svcNamespace
}

// getLabelsMapFromKeyValue returns map[string]string for given array of vim25types.KeyValue
func getLabelsMapFromKeyValue(labels []vim25types.KeyValue) map[string]string {
	labelsMap := make(map[string]string)
	for _, label := range labels {
		labelsMap[label.Key] = label.Value
	}
	return labelsMap
}

// getDatastoreByURL returns the *Datastore instance given its URL.
func getDatastoreByURL(ctx context.Context, datastoreURL string, dc *object.Datacenter) (*object.Datastore, error) {
	finder := find.NewFinder(dc.Client(), false)
	finder.SetDatacenter(dc)
	datastores, err := finder.DatastoreList(ctx, "*")
	if err != nil {
		framework.Logf("failed to get all the datastores. err: %+v", err)
		return nil, err
	}
	var dsList []types.ManagedObjectReference
	for _, ds := range datastores {
		dsList = append(dsList, ds.Reference())
	}

	var dsMoList []mo.Datastore
	pc := property.DefaultCollector(dc.Client())
	properties := []string{"info"}
	err = pc.Retrieve(ctx, dsList, properties, &dsMoList)
	if err != nil {
		framework.Logf("failed to get Datastore managed objects from datastore objects."+
			" dsObjList: %+v, properties: %+v, err: %v", dsList, properties, err)
		return nil, err
	}
	for _, dsMo := range dsMoList {
		if dsMo.Info.GetDatastoreInfo().Url == datastoreURL {
			return object.NewDatastore(dc.Client(),
				dsMo.Reference()), nil
		}
	}
	err = fmt.Errorf("Couldn't find Datastore given URL %q", datastoreURL)
	return nil, err
}

// getPersistentVolumeClaimSpec gets vsphere persistent volume spec with given selector labels
// and binds it to given pv
func getPersistentVolumeClaimSpec(namespace string, labels map[string]string, pvName string) *v1.PersistentVolumeClaim {
	var (
		pvc *v1.PersistentVolumeClaim
	)
	sc := ""
	pvc = &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-",
			Namespace:    namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse("2Gi"),
				},
			},
			VolumeName:       pvName,
			StorageClassName: &sc,
		},
	}
	if labels != nil {
		pvc.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}

	return pvc
}

// function to create PV volume spec with given FCD ID, Reclaim Policy and labels
func getPersistentVolumeSpec(fcdID string, persistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy, labels map[string]string) *v1.PersistentVolume {
	var (
		pvConfig fpv.PersistentVolumeConfig
		pv       *v1.PersistentVolume
		claimRef *v1.ObjectReference
	)
	pvConfig = fpv.PersistentVolumeConfig{
		NamePrefix: "vspherepv-",
		PVSource: v1.PersistentVolumeSource{
			CSI: &v1.CSIPersistentVolumeSource{
				Driver:       e2evSphereCSIBlockDriverName,
				VolumeHandle: fcdID,
				ReadOnly:     false,
				FSType:       "ext4",
			},
		},
		Prebind: nil,
	}

	pv = &v1.PersistentVolume{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pvConfig.NamePrefix,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: persistentVolumeReclaimPolicy,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): resource.MustParse("2Gi"),
			},
			PersistentVolumeSource: pvConfig.PVSource,
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			ClaimRef:         claimRef,
			StorageClassName: "",
		},
		Status: v1.PersistentVolumeStatus{},
	}
	if labels != nil {
		pv.Labels = labels
	}
	// Annotation needed to delete a statically created pv
	annotations := make(map[string]string)
	annotations["pv.kubernetes.io/provisioned-by"] = e2evSphereCSIBlockDriverName
	pv.Annotations = annotations
	return pv
}

// invokeVCenterServiceControl invokes the given command for the given service
// via service-control on the given vCenter host over SSH.
func invokeVCenterServiceControl(command, service, host string) error {
	sshCmd := fmt.Sprintf("service-control --%s %s", command, service)
	framework.Logf("Invoking command %v on vCenter host %v", sshCmd, host)
	result, err := fssh.SSH(sshCmd, host, framework.TestContext.Provider)
	if err != nil || result.Code != 0 {
		fssh.LogResult(result)
		return fmt.Errorf("couldn't execute command: %s on vCenter host: %v", sshCmd, err)
	}
	return nil
}

// replacePasswordRotationTime invokes the given command to replace the password rotation time to 0,
// so that password roation happens immediately
// on the given vCenter host over SSH, vmon-cli is used to restart the wcp service after changing the time
func replacePasswordRotationTime(file, host string) error {
	sshCmd := fmt.Sprintf("sed -i '3 c\\0' %s", file)
	framework.Logf("Invoking command %v on vCenter host %v", sshCmd, host)
	result, err := fssh.SSH(sshCmd, host, framework.TestContext.Provider)
	if err != nil || result.Code != 0 {
		fssh.LogResult(result)
		return fmt.Errorf("couldn't execute command: %s on vCenter host: %v", sshCmd, err)
	}

	sshCmd = fmt.Sprintf("vmon-cli -r %s", wcpServiceName)
	framework.Logf("Invoking command %v on vCenter host %v", sshCmd, host)
	result, err = fssh.SSH(sshCmd, host, framework.TestContext.Provider)
	time.Sleep(sleepTimeOut)
	if err != nil || result.Code != 0 {
		fssh.LogResult(result)
		return fmt.Errorf("couldn't execute command: %s on vCenter host: %v", sshCmd, err)
	}
	return nil
}

// writeToFile will take two parameters:
// 1. the absolute path of the file(including filename) to be created
// 2. data content to be written into the file
// Returns nil on Success and error on failure
func writeToFile(filePath, data string) error {
	if filePath == "" {
		return fmt.Errorf("invalid filename")
	}
	f, err := os.Create(filePath)
	if err != nil {
		framework.Logf("Error: %v", err)
		return err
	}
	_, err = f.WriteString(data)
	if err != nil {
		framework.Logf("Error: %v", err)
		f.Close()
		return err
	}
	err = f.Close()
	if err != nil {
		framework.Logf("Error: %v", err)
		return err
	}
	return nil
}

// invokeVCenterChangePassword invokes `dir-cli password reset` command on the given vCenter host over SSH
// thereby resetting the currentPassword of the `user` to the `newPassword`
func invokeVCenterChangePassword(user, adminPassword, newPassword, host string) error {
	// create an input file and write passwords into it
	path := "input.txt"
	data := fmt.Sprintf("%s\n%s\n", adminPassword, newPassword)
	err := writeToFile(path, data)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	defer func() {
		// delete the input file containing passwords
		err = os.Remove(path)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}()
	// remote copy this input file to VC
	copyCmd := fmt.Sprintf("/bin/cat %s | /usr/bin/ssh root@%s '/usr/bin/cat >> input_copy.txt'", path, e2eVSphere.Config.Global.VCenterHostname)
	fmt.Printf("Executing the command: %s\n", copyCmd)
	_, err = exec.Command("/bin/sh", "-c", copyCmd).Output()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	defer func() {
		// remove the input_copy.txt file from VC
		removeCmd := fmt.Sprintf("/usr/bin/ssh root@%s '/usr/bin/rm input_copy.txt'", e2eVSphere.Config.Global.VCenterHostname)
		_, err = exec.Command("/bin/sh", "-c", removeCmd).Output()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}()

	sshCmd := fmt.Sprintf("/usr/bin/cat input_copy.txt | /usr/lib/vmware-vmafd/bin/dir-cli password reset --account %s", user)
	framework.Logf("Invoking command %v on vCenter host %v", sshCmd, host)
	result, err := fssh.SSH(sshCmd, host, framework.TestContext.Provider)
	if err != nil || result.Code != 0 {
		fssh.LogResult(result)
		return fmt.Errorf("couldn't execute command: %s on vCenter host: %v", sshCmd, err)
	}
	if !strings.Contains(result.Stdout, "Password was reset successfully for ") {
		framework.Logf("failed to change the password for user %s: %s", user, result.Stdout)
		return err
	}
	framework.Logf("password changed successfully for user: %s", user)
	return nil
}

// verifyVolumeTopology verifies that the Node Affinity rules in the volume
// match the topology constraints specified in the storage class
func verifyVolumeTopology(pv *v1.PersistentVolume, zoneValues []string, regionValues []string) (string, string, error) {
	if pv.Spec.NodeAffinity == nil || len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		return "", "", fmt.Errorf("Node Affinity rules for PV should exist in topology aware provisioning")
	}
	var pvZone string
	var pvRegion string
	for _, labels := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions {
		if labels.Key == zoneKey {
			for _, value := range labels.Values {
				gomega.Expect(zoneValues).To(gomega.ContainElement(value), fmt.Sprintf("Node Affinity rules for PV %s: %v does not contain zone specified in storage class %v", pv.Name, value, zoneValues))
				pvZone = value
			}
		}
		if labels.Key == regionKey {
			for _, value := range labels.Values {
				gomega.Expect(regionValues).To(gomega.ContainElement(value), fmt.Sprintf("Node Affinity rules for PV %s: %v does not contain region specified in storage class %v", pv.Name, value, regionValues))
				pvRegion = value
			}
		}
	}
	framework.Logf("PV %s is located in zone: %s and region: %s", pv.Name, pvZone, pvRegion)
	return pvRegion, pvZone, nil
}

// verifyPodLocation verifies that a pod is scheduled on
// a node that belongs to the topology on which PV is provisioned
func verifyPodLocation(pod *v1.Pod, nodeList *v1.NodeList, zoneValue string, regionValue string) error {
	for _, node := range nodeList.Items {
		if pod.Spec.NodeName == node.Name {
			for labelKey, labelValue := range node.Labels {
				if labelKey == zoneKey && zoneValue != "" {
					gomega.Expect(zoneValue).To(gomega.Equal(labelValue), fmt.Sprintf("Pod %s is not running on Node located in zone %v", pod.Name, zoneValue))
				}
				if labelKey == regionKey && regionValue != "" {
					gomega.Expect(regionValue).To(gomega.Equal(labelValue), fmt.Sprintf("Pod %s is not running on Node located in region %v", pod.Name, regionValue))
				}
			}
		}
	}
	return nil
}

// getTopologyFromPod rturn topology value from node affinity information
func getTopologyFromPod(pod *v1.Pod, nodeList *v1.NodeList) (string, string, error) {
	for _, node := range nodeList.Items {
		if pod.Spec.NodeName == node.Name {
			podRegion := node.Labels[v1.LabelZoneRegion]
			podZone := node.Labels[v1.LabelZoneFailureDomain]
			return podRegion, podZone, nil
		}
	}
	err := errors.New("Could not find the topology from pod")
	return "", "", err
}

// topologyParameterForStorageClass creates a topology map using the topology values ENV variables
// Returns the allowedTopologies parameters required for the Storage Class
// Input : <region-1>:<zone-1>, <region-1>:<zone-2>
// Output : [region-1], [zone-1, zone-2] {region-1: zone-1, region-1:zone-2}
func topologyParameterForStorageClass(topology string) ([]string, []string, []v1.TopologySelectorLabelRequirement) {
	topologyMap := createTopologyMap(topology)
	regionValues, zoneValues := getValidTopology(topologyMap)
	allowedTopologies := []v1.TopologySelectorLabelRequirement{
		{
			Key:    regionKey,
			Values: regionValues,
		},
		{
			Key:    zoneKey,
			Values: zoneValues,
		},
	}
	return regionValues, zoneValues, allowedTopologies
}

// createTopologyMap strips the topology string provided in the environment variable
// into a map from region to zones
// example envTopology = "r1:z1,r1:z2,r2:z3"
func createTopologyMap(topologyString string) map[string][]string {
	topologyMap := make(map[string][]string)
	for _, t := range strings.Split(topologyString, ",") {
		t = strings.TrimSpace(t)
		topology := strings.Split(t, ":")
		if len(topology) != 2 {
			continue
		}
		topologyMap[topology[0]] = append(topologyMap[topology[0]], topology[1])
	}
	return topologyMap
}

// getValidTopology returns the regions and zones from the input topology map
// so that they can be provided to a storage class
func getValidTopology(topologyMap map[string][]string) ([]string, []string) {
	var regionValues []string
	var zoneValues []string
	for region, zones := range topologyMap {
		regionValues = append(regionValues, region)
		zoneValues = append(zoneValues, zones...)
	}
	return regionValues, zoneValues
}

// createResourceQuota creates resource quota for the specified namespace.
func createResourceQuota(client clientset.Interface, namespace string, size string, scName string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitTime := 15
	//deleteResourceQuota if already present
	deleteResourceQuota(client, namespace)

	resourceQuota := newTestResourceQuota(quotaName, size, scName)
	resourceQuota, err := client.CoreV1().ResourceQuotas(namespace).Create(ctx, resourceQuota, metav1.CreateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.By(fmt.Sprintf("Create Resource quota: %+v", resourceQuota))
	ginkgo.By(fmt.Sprintf("Waiting for %v seconds to allow resourceQuota to be claimed", waitTime))
	time.Sleep(time.Duration(waitTime) * time.Second)
}

// deleteResourceQuota deletes resource quota for the specified namespace, if it exists.
func deleteResourceQuota(client clientset.Interface, namespace string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := client.CoreV1().ResourceQuotas(namespace).Get(ctx, quotaName, metav1.GetOptions{})
	if err == nil {
		err = client.CoreV1().ResourceQuotas(namespace).Delete(ctx, quotaName, *metav1.NewDeleteOptions(0))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By(fmt.Sprintf("Deleted Resource quota: %+v", quotaName))
	}
}

// newTestResourceQuota returns a quota that enforces default constraints for testing
func newTestResourceQuota(name string, size string, scName string) *v1.ResourceQuota {
	hard := v1.ResourceList{}
	// test quota on discovered resource type
	hard[v1.ResourceName(scName+rqStorageType)] = resource.MustParse(size)
	return &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.ResourceQuotaSpec{Hard: hard},
	}
}

// checkEventsforError prints the list of all events that occurred in the namespace and
// searches for expectedErrorMsg among these events
func checkEventsforError(client clientset.Interface, namespace string, listOptions metav1.ListOptions, expectedErrorMsg string) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventList, _ := client.CoreV1().Events(namespace).List(ctx, listOptions)
	isFailureFound := false
	for _, item := range eventList.Items {
		ginkgo.By(fmt.Sprintf("EventList item: %q \n", item.Message))
		if strings.Contains(item.Message, expectedErrorMsg) {
			isFailureFound = true
			break
		}
	}
	return isFailureFound
}

// getNamespaceToRunTests returns the namespace in which the tests are expected to run
// For Vanilla & GuestCluster test setups: Returns random namespace name generated by the framework
// For SupervisorCluster test setup: Returns the user created namespace where pod vms will be provisioned
func getNamespaceToRunTests(f *framework.Framework) string {
	if supervisorCluster {
		return GetAndExpectStringEnvVar(envSupervisorClusterNamespace)
	}
	return f.Namespace.Name
}

// getPVCFromSupervisorCluster takes name of the persistentVolumeClaim as input
// returns the corresponding persistentVolumeClaim object
func getPVCFromSupervisorCluster(pvcName string) *v1.PersistentVolumeClaim {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svcClient, svcNamespace := getSvcClientAndNamespace()
	pvc, err := svcClient.CoreV1().PersistentVolumeClaims(svcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(pvc).NotTo(gomega.BeNil())
	return pvc
}

// getVolumeIDFromSupervisorCluster returns SV PV volume handle for given SVC PVC name
func getVolumeIDFromSupervisorCluster(pvcName string) string {
	var svcClient clientset.Interface
	var err error
	if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
		svcClient, err = k8s.CreateKubernetesClientFromConfig(k8senv)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	svNamespace := GetAndExpectStringEnvVar(envSupervisorClusterNamespace)
	svcPV := getPvFromClaim(svcClient, svNamespace, pvcName)
	volumeHandle := svcPV.Spec.CSI.VolumeHandle
	ginkgo.By(fmt.Sprintf("Found volume in Supervisor cluster with VolumeID: %s", volumeHandle))
	return volumeHandle
}

// getPvFromSupervisorCluster returns SV PV for given SVC PVC name
func getPvFromSupervisorCluster(pvcName string) *v1.PersistentVolume {
	var svcClient clientset.Interface
	var err error
	if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
		svcClient, err = k8s.CreateKubernetesClientFromConfig(k8senv)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	svNamespace := GetAndExpectStringEnvVar(envSupervisorClusterNamespace)
	svcPV := getPvFromClaim(svcClient, svNamespace, pvcName)
	return svcPV
}

func verifyFilesExistOnVSphereVolume(namespace string, podName string, filePaths ...string) {
	for _, filePath := range filePaths {
		_, err := framework.RunKubectl(namespace, "exec", fmt.Sprintf("--namespace=%s", namespace), podName, "--", "/bin/ls", filePath)
		framework.ExpectNoError(err, fmt.Sprintf("failed to verify file: %q on the pod: %q", filePath, podName))
	}
}

func createEmptyFilesOnVSphereVolume(namespace string, podName string, filePaths []string) {
	for _, filePath := range filePaths {
		err := framework.CreateEmptyFileOnPod(namespace, podName, filePath)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
}

// CreateService creates a k8s service as described in the service.yaml present in the manifest path and returns that
// service to the caller
func CreateService(ns string, c clientset.Interface) *v1.Service {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svcManifestFilePath := filepath.Join(manifestPath, "service.yaml")
	framework.Logf("Parsing service from %v", svcManifestFilePath)
	svc, err := manifest.SvcFromManifest(svcManifestFilePath)
	framework.ExpectNoError(err)

	service, err := c.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	return service
}

// deleteService deletes a k8s service
func deleteService(ns string, c clientset.Interface, service *v1.Service) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.CoreV1().Services(ns).Delete(ctx, service.Name, *metav1.NewDeleteOptions(0))
	framework.ExpectNoError(err)
}

// GetStatefulSetFromManifest creates a StatefulSet from the statefulset.yaml file present in the manifest path
func GetStatefulSetFromManifest(ns string) *appsv1.StatefulSet {
	ssManifestFilePath := filepath.Join(manifestPath, "statefulset.yaml")
	framework.Logf("Parsing statefulset from %v", ssManifestFilePath)
	ss, err := manifest.StatefulSetFromManifest(ssManifestFilePath, ns)
	framework.ExpectNoError(err)
	return ss
}

// isDatastoreBelongsToDatacenterSpecifiedInConfig checks whether the given datastoreURL belongs to the datacenter specified in the vSphere.conf file
func isDatastoreBelongsToDatacenterSpecifiedInConfig(datastoreURL string) bool {
	var datacenters []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finder := find.NewFinder(e2eVSphere.Client.Client, false)
	cfg, err := getConfig()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	dcList := strings.Split(cfg.Global.Datacenters, ",")
	for _, dc := range dcList {
		dcName := strings.TrimSpace(dc)
		if dcName != "" {
			datacenters = append(datacenters, dcName)
		}
	}
	for _, dc := range datacenters {
		defaultDatacenter, _ := finder.Datacenter(ctx, dc)
		finder.SetDatacenter(defaultDatacenter)
		defaultDatastore, err := getDatastoreByURL(ctx, datastoreURL, defaultDatacenter)
		if defaultDatastore != nil && err == nil {
			return true
		}
	}

	// loop through all datacenters specified in conf file, and cannot find this given datastore
	return false
}

func getTargetvSANFileShareDatastoreURLsFromConfig() []string {
	var targetDsURLs []string
	cfg, err := getConfig()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	if cfg.Global.TargetvSANFileShareDatastoreURLs != "" {
		targetDsURLs = strings.Split(cfg.Global.TargetvSANFileShareDatastoreURLs, ",")
	}
	return targetDsURLs
}

func isDatastorePresentinTargetvSANFileShareDatastoreURLs(datastoreURL string) bool {
	cfg, err := getConfig()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	datastoreURL = strings.TrimSpace(datastoreURL)
	targetDatastoreUrls := strings.Split(cfg.Global.TargetvSANFileShareDatastoreURLs, ",")
	for _, dsURL := range targetDatastoreUrls {
		dsURL = strings.TrimSpace(dsURL)
		if datastoreURL == dsURL {
			return true
		}
	}
	return false
}

func verifyVolumeExistInSupervisorCluster(pvcName string) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var svcClient clientset.Interface
	var err error
	if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
		svcClient, err = k8s.CreateKubernetesClientFromConfig(k8senv)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	svNamespace := GetAndExpectStringEnvVar(envSupervisorClusterNamespace)
	_, err = svcClient.CoreV1().PersistentVolumeClaims(svNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	return err == nil
}

// verifyCRDInSupervisor is a helper method to check if a given crd is created/deleted in the supervisor cluster
// This method will fetch the List of CRD Objects for a given crdName, Version and Group and then
// verifies if the given expectedInstanceName exist in the list
func verifyCRDInSupervisor(ctx context.Context, f *framework.Framework, expectedInstanceName string, crdName string, crdVersion string, crdGroup string, isCreated bool) {
	k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG")
	cfg, err := clientcmd.BuildConfigFromFlags("", k8senv)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	dynamicClient, err := dynamic.NewForConfig(cfg)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gvr := schema.GroupVersionResource{Group: crdGroup, Version: crdVersion, Resource: crdName}
	resourceClient := dynamicClient.Resource(gvr).Namespace("")
	list, err := resourceClient.List(ctx, metav1.ListOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	var instanceFound bool
	for _, crd := range list.Items {
		if crdName == "cnsnodevmattachments" {
			instance := &cnsnodevmattachmentv1alpha1.CnsNodeVmAttachment{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(crd.Object, instance)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if expectedInstanceName == instance.Name {
				ginkgo.By(fmt.Sprintf("Found CNSNodeVMAttachment crd: %v, expected: %v", instance, expectedInstanceName))
				instanceFound = true
				break
			}

		}
		if crdName == "cnsvolumemetadatas" {
			instance := &cnsvolumemetadatav1alpha1.CnsVolumeMetadata{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(crd.Object, instance)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if expectedInstanceName == instance.Name {
				ginkgo.By(fmt.Sprintf("Found CNSVolumeMetadata crd: %v, expected: %v", instance, expectedInstanceName))
				instanceFound = true
				break
			}
		}
	}
	if isCreated {
		gomega.Expect(instanceFound).To(gomega.BeTrue())
	} else {
		gomega.Expect(instanceFound).To(gomega.BeFalse())
	}
}

// trimQuotes takes a quoted string as input and returns the same string unquoted
func trimQuotes(str string) string {
	str = strings.TrimPrefix(str, "\"")
	str = strings.TrimSuffix(str, "\"")
	return str
}

/*
	readConfigFromSecretString takes input string of the form:
		[Global]
		insecure-flag = "true"
		cluster-id = "domain-c1047"
		[VirtualCenter "wdc-rdops-vm09-dhcp-238-224.eng.vmware.com"]
		user = "workload_storage_management-792c9cce-3cd2-4618-8853-52f521400e05@vsphere.local"
		password = "qd?\\/\"K=O_<ZQw~s4g(S"
		datacenters = "datacenter-1033"
		port = "443"
	Returns a de-serialized structured config data
*/
func readConfigFromSecretString(cfg string) (e2eTestConfig, error) {
	var config e2eTestConfig
	key, value := "", ""
	lines := strings.Split(cfg, "\n")
	for index, line := range lines {
		if index == 0 {
			// Skip [Global]
			continue
		}
		words := strings.Split(line, " = ")
		if len(words) == 1 {
			// case VirtualCenter
			words = strings.Split(line, " ")
			if strings.Contains(words[0], "VirtualCenter") {
				value = words[1]
				// Remove trailing '"]' characters from value
				value = strings.TrimSuffix(value, "]")
				config.Global.VCenterHostname = trimQuotes(value)
				fmt.Printf("Key: VirtualCenter, Value: %s\n", value)
			}
			continue
		}
		key = words[0]
		value = trimQuotes(words[1])
		var strconvErr error
		switch key {
		case "insecure-flag":
			if strings.Contains(value, "true") {
				config.Global.InsecureFlag = true
			} else {
				config.Global.InsecureFlag = false
			}
		case "cluster-id":
			config.Global.ClusterID = value
		case "user":
			config.Global.User = value
		case "password":
			config.Global.Password = value
		case "datacenters":
			config.Global.Datacenters = value
		case "port":
			config.Global.VCenterPort = value
		case "cnsregistervolumes-cleanup-intervalinmin":
			config.Global.CnsRegisterVolumesCleanupIntervalInMin, strconvErr = strconv.Atoi(value)
			gomega.Expect(strconvErr).NotTo(gomega.HaveOccurred())
		default:
			return config, fmt.Errorf("unknown key %s in the input string", key)
		}
	}
	return config, nil
}

// writeConfigToSecretString takes in a structured config data and serializes that into a string
func writeConfigToSecretString(cfg e2eTestConfig) (string, error) {
	result := fmt.Sprintf("[Global]\ninsecure-flag = \"%t\"\n[VirtualCenter \"%s\"]\nuser = \"%s\"\npassword = \"%s\"\ndatacenters = \"%s\"\nport = \"%s\"\n",
		cfg.Global.InsecureFlag, cfg.Global.VCenterHostname, cfg.Global.User, cfg.Global.Password, cfg.Global.Datacenters, cfg.Global.VCenterPort)
	return result, nil
}

// Function to create CnsRegisterVolume spec, with given FCD ID and PVC name
func getCNSRegisterVolumeSpec(ctx context.Context, namespace string, fcdID string, persistentVolumeClaimName string, accessMode v1.PersistentVolumeAccessMode) *cnsregistervolumev1alpha1.CnsRegisterVolume {
	var (
		cnsRegisterVolume *cnsregistervolumev1alpha1.CnsRegisterVolume
	)
	log := logger.GetLogger(ctx)
	log.Infof("get CNSRegisterVolume spec")
	cnsRegisterVolume = &cnsregistervolumev1alpha1.CnsRegisterVolume{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "cnsregvol-",
			Namespace:    namespace,
		},
		Spec: cnsregistervolumev1alpha1.CnsRegisterVolumeSpec{
			PvcName:  persistentVolumeClaimName,
			VolumeID: fcdID,
			AccessMode: v1.PersistentVolumeAccessMode(
				accessMode,
			),
		},
	}
	return cnsRegisterVolume
}

//Create CNS register volume
func createCNSRegisterVolume(ctx context.Context, restConfig *rest.Config, cnsRegisterVolume *cnsregistervolumev1alpha1.CnsRegisterVolume) error {
	log := logger.GetLogger(ctx)

	cnsOperatorClient, err := k8s.NewClientForGroup(ctx, restConfig, cnsoperatorv1alpha1.GroupName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	log.Infof("Create CNSRegisterVolume")
	err = cnsOperatorClient.Create(ctx, cnsRegisterVolume)

	return err
}

//Query CNS Register volume . Returns true if the CNSRegisterVolume is available otherwise false
func queryCNSRegisterVolume(ctx context.Context, restClientConfig *rest.Config, cnsRegistervolumeName string, namespace string) bool {
	isPresent := false
	log := logger.GetLogger(ctx)
	log.Infof("cleanUpCnsRegisterVolumeInstances: start")
	cnsOperatorClient, err := k8s.NewClientForGroup(ctx, restClientConfig, cnsoperatorv1alpha1.GroupName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	// Get list of CnsRegisterVolume instances from all namespaces
	cnsRegisterVolumesList := &cnsregistervolumev1alpha1.CnsRegisterVolumeList{}
	err = cnsOperatorClient.List(ctx, cnsRegisterVolumesList)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	cns := &cnsregistervolumev1alpha1.CnsRegisterVolume{}
	err = cnsOperatorClient.Get(ctx, pkgtypes.NamespacedName{Name: cnsRegistervolumeName, Namespace: namespace}, cns)
	if err == nil {
		log.Infof("CNS RegisterVolume %s Found in the namespace  %s:", cnsRegistervolumeName, namespace)
		isPresent = true
	}

	return isPresent

}

//Verify Bi-directional referance of Pv and PVC in case of static volume provisioning
func verifyBidirectionalReferenceOfPVandPVC(ctx context.Context, client clientset.Interface, pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume, fcdID string) {
	log := logger.GetLogger(ctx)

	pvcName := pvc.GetName()
	pvcNamespace := pvc.GetNamespace()
	pvcvolume := pvc.Spec.VolumeName
	pvccapacity := pvc.Status.Capacity.StorageEphemeral().Size()

	pvName := pv.GetName()
	pvClaimRefName := pv.Spec.ClaimRef.Name
	pvClaimRefNamespace := pv.Spec.ClaimRef.Namespace
	pvcapacity := pv.Spec.Capacity.StorageEphemeral().Size()
	pvvolumeHandle := pv.Spec.PersistentVolumeSource.CSI.VolumeHandle

	if pvClaimRefName != pvcName && pvClaimRefNamespace != pvcNamespace {
		log.Infof("PVC Name :%s PVC namespace : %s", pvcName, pvcNamespace)
		log.Infof("PV Name :%s PVnamespace : %s", pvcName, pvcNamespace)
		log.Errorf("Mismatch in PV and PVC name and namespace")
	}

	if pvcvolume != pvName {
		log.Errorf("PVC volume :%s PV name : %s expected to be same", pvcvolume, pvName)
	}

	if pvvolumeHandle != fcdID {
		log.Errorf("Mismatch in PV volumeHandle:%s and the actual fcdID: %s", pvvolumeHandle, fcdID)
	}

	if pvccapacity != pvcapacity {
		log.Errorf("Mismatch in pv capacity:%d and pvc capacity: %d", pvcapacity, pvccapacity)
	}
}

//Get CNS register volume
func getCNSRegistervolume(ctx context.Context, restClientConfig *rest.Config, cnsRegisterVolume *v1alpha1.CnsRegisterVolume) *v1alpha1.CnsRegisterVolume {
	cnsOperatorClient, err := k8s.NewClientForGroup(ctx, restClientConfig, cnsoperatorv1alpha1.GroupName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	cns := &v1alpha1.CnsRegisterVolume{}
	err = cnsOperatorClient.Get(ctx, pkgtypes.NamespacedName{Name: cnsRegisterVolume.Name, Namespace: cnsRegisterVolume.Namespace}, cns)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	return cns
}

// Update CNS register volume
func updateCNSRegistervolume(ctx context.Context, restClientConfig *rest.Config, cnsRegisterVolume *v1alpha1.CnsRegisterVolume) *v1alpha1.CnsRegisterVolume {
	cnsOperatorClient, err := k8s.NewClientForGroup(ctx, restClientConfig, cnsoperatorv1alpha1.GroupName)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	err = cnsOperatorClient.Update(ctx, cnsRegisterVolume)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	return cnsRegisterVolume

}

// CreatePodByUserID with given claims based on node selector. This method is addition to CreatePod method.
// Here userID can be specified for pod user
func CreatePodByUserID(client clientset.Interface, namespace string, nodeSelector map[string]string, pvclaims []*v1.PersistentVolumeClaim, isPrivileged bool, command string, userID int64) (*v1.Pod, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pod := GetPodSpecByUserID(namespace, nodeSelector, pvclaims, isPrivileged, command, userID)
	pod, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Pod creation failed")
	if err != nil {
		return nil, fmt.Errorf("pod Create API error: %v", err)
	}
	// Waiting for pod to be running
	err = fpod.WaitForPodNameRunningInNamespace(client, pod.Name, namespace)
	if err != nil {
		return pod, fmt.Errorf("pod %q is not Running: %v", pod.Name, err)
	}
	// get fresh pod info
	pod, err = client.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return pod, fmt.Errorf("pod Get API error: %v", err)
	}
	return pod, nil
}

// GetPodSpecByUserID returns a pod definition based on the namespace. The pod references the PVC's
// name.
// This method is addition to MakePod method.
// Here userID can be specified for pod user
func GetPodSpecByUserID(ns string, nodeSelector map[string]string, pvclaims []*v1.PersistentVolumeClaim, isPrivileged bool, command string, userID int64) *v1.Pod {
	if len(command) == 0 {
		command = "trap exit TERM; while true; do sleep 1; done"
	}
	podSpec := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-tester-",
			Namespace:    ns,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "write-pod",
					Image:   busyBoxImageOnGcr,
					Command: []string{"/bin/sh"},
					Args:    []string{"-c", command},
					SecurityContext: &v1.SecurityContext{
						Privileged: &isPrivileged,
						RunAsUser:  &userID,
					},
				},
			},
			RestartPolicy: v1.RestartPolicyOnFailure,
		},
	}
	var volumeMounts = make([]v1.VolumeMount, len(pvclaims))
	var volumes = make([]v1.Volume, len(pvclaims))
	for index, pvclaim := range pvclaims {
		volumename := fmt.Sprintf("volume%v", index+1)
		volumeMounts[index] = v1.VolumeMount{Name: volumename, MountPath: "/mnt/" + volumename}
		volumes[index] = v1.Volume{Name: volumename, VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: pvclaim.Name, ReadOnly: false}}}
	}
	podSpec.Spec.Containers[0].VolumeMounts = volumeMounts
	podSpec.Spec.Volumes = volumes
	if nodeSelector != nil {
		podSpec.Spec.NodeSelector = nodeSelector
	}
	return podSpec
}

/*
writeDataOnFileFromPod writes specified data from given Pod at the given
*/
func writeDataOnFileFromPod(namespace string, podName string, filePath string, data string) {
	_, err := framework.RunKubectl(namespace, "exec", fmt.Sprintf("--namespace=%s", namespace), podName, "--", "/bin/sh", "-c", fmt.Sprintf(" echo %s >  %s ", data, filePath))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

/*
readFileFromPod read data from given Pod and the given file
*/
func readFileFromPod(namespace string, podName string, filePath string) string {
	output, err := framework.RunKubectl(namespace, "exec", fmt.Sprintf("--namespace=%s", namespace), podName, "--", "/bin/sh", "-c", fmt.Sprintf("less  %s", filePath))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return output
}

// getPersistentVolumeClaimSpecFromVolume gets vsphere persistent volume spec with given selector labels
// and binds it to given pv
func getPersistentVolumeClaimSpecFromVolume(namespace string, pvName string, labels map[string]string, accessMode v1.PersistentVolumeAccessMode) *v1.PersistentVolumeClaim {
	var (
		pvc *v1.PersistentVolumeClaim
	)
	sc := ""
	pvc = &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pvc-",
			Namespace:    namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				accessMode,
			},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse("2Gi"),
				},
			},
			VolumeName:       pvName,
			StorageClassName: &sc,
		},
	}
	if labels != nil {
		pvc.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}

	return pvc
}

// getPersistentVolumeSpecFromVolume gets static PV volume spec with given Volume ID, Reclaim Policy and labels
func getPersistentVolumeSpecFromVolume(volumeID string, persistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy, labels map[string]string, accessMode v1.PersistentVolumeAccessMode) *v1.PersistentVolume {
	var (
		pvConfig fpv.PersistentVolumeConfig
		pv       *v1.PersistentVolume
		claimRef *v1.ObjectReference
	)
	pvConfig = fpv.PersistentVolumeConfig{
		NamePrefix: "vspherepv-",
		PVSource: v1.PersistentVolumeSource{
			CSI: &v1.CSIPersistentVolumeSource{
				Driver:       e2evSphereCSIBlockDriverName,
				VolumeHandle: volumeID,
				FSType:       "nfs4",
				ReadOnly:     true,
			},
		},
		Prebind: nil,
	}
	// Annotation needed to delete a statically created pv
	annotations := make(map[string]string)
	annotations["pv.kubernetes.io/provisioned-by"] = e2evSphereCSIBlockDriverName

	pv = &v1.PersistentVolume{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pvConfig.NamePrefix,
			Annotations:  annotations,
			Labels:       labels,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: persistentVolumeReclaimPolicy,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): resource.MustParse("2Gi"),
			},
			PersistentVolumeSource: pvConfig.PVSource,
			AccessModes: []v1.PersistentVolumeAccessMode{
				accessMode,
			},
			ClaimRef:         claimRef,
			StorageClassName: "",
		},
		Status: v1.PersistentVolumeStatus{},
	}
	return pv
}

// DeleteStatefulPodAtIndex deletes pod given index in the desired statefulset
func DeleteStatefulPodAtIndex(client clientset.Interface, index int, ss *apps.StatefulSet) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	name := fmt.Sprintf("%v-%v", ss.Name, index)
	noGrace := int64(0)
	if err := client.CoreV1().Pods(ss.Namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &noGrace}); err != nil {
		framework.Failf("Failed to delete stateful pod %v for StatefulSet %v/%v: %v", name, ss.Namespace, ss.Name, err)
	}

}

// createPod with given claims based on node selector
func createPod(client clientset.Interface, namespace string, nodeSelector map[string]string, pvclaims []*v1.PersistentVolumeClaim, isPrivileged bool, command string) (*v1.Pod, error) {
	pod := fpod.MakePod(namespace, nodeSelector, pvclaims, isPrivileged, command)
	pod.Spec.Containers[0].Image = busyBoxImageOnGcr
	pod, err := client.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return pod, fmt.Errorf("pod Create API error: %v", err)
	}
	// Waiting for pod to be running
	err = fpod.WaitForPodNameRunningInNamespace(client, pod.Name, namespace)
	if err != nil {
		return pod, fmt.Errorf("pod %q is not Running: %v", pod.Name, err)
	}
	// get fresh pod info
	pod, err = client.CoreV1().Pods(namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err != nil {
		return pod, fmt.Errorf("pod Get API error: %v", err)
	}
	return pod, nil
}
