# Windows prototype
This fork contains a WIP Windows prototype. Until privileged containers are supported in Windows, [csi-proxy](https://github.com/kubernetes-csi/csi-proxy) is used to execute commands on the host from the csi-node pod.  


#### Requirements / Steps
* Set up a vSphere cluster with vsphere-csi (v2.1.0) and Windows nodes
* Ensure [csi-proxy](https://github.com/kubernetes-csi/csi-proxy) is running on each Windows node with [this patch](https://github.com/kubernetes-csi/csi-proxy/pull/110) included. This is needed to correctly identify disks on vSphere.
* Patch existing csi-node DaemonSet to only target Linux nodes: 
```kubectl patch daemonset -n kube-system vsphere-csi-node -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/os": "linux"}}}}}' ```
* Apply [Windows DaemonSet](manifests/v2.1.0/vsphere-67u3/vanilla/deploy/vsphere-csi-node-windows-ds.yaml)
```kubectl apply -f https://raw.githubusercontent.com/gab-satchi/vsphere-csi-driver/windows-prototype/manifests/v2.1.0/vsphere-67u3/vanilla/deploy/vsphere-csi-node-windows-ds.yaml```
* Apply Windows yaml files from the [examples](example/vanilla-k8s-block-driver/)

#### Known Issues
* Creating files at the root directory gets an access denied. Creating folders works as expected and files can be created within the folders

# Container Storage Interface (CSI) driver for vSphere

This repository provides tools and scripts for building and testing the vSphere CSI provider. This driver is in a stable `GA` state and is suitable for production use. It currently requires vSphere 6.7 U3 or higher in order to operate.

The CSI driver, when used on Kubernetes, also requires the use of the out-of-tree vSphere Cloud Provider Interface [CPI](https://github.com/kubernetes/cloud-provider-vsphere).

The driver has been tested with, and is supported on, K8s 1.14 and above.

## Documentation

Documentation for vSphere CSI Driver is available here:

* <https://vsphere-csi-driver.sigs.k8s.io>

## vSphere CSI Driver Images

Please use appropriate deployment yaml files available here - <https://github.com/kubernetes-sigs/vsphere-csi-driver/tree/master/manifests>

Note:

* `v1.0.2`, deployment yamls files are compatible with `v1.0.1`.
* It is recommended to use `v2.0.1` if you're on `vSphere 6.7 Update3`

### v2.0.1

* gcr.io/cloud-provider-vsphere/csi/release/driver:v2.0.1
* gcr.io/cloud-provider-vsphere/csi/release/syncer:v2.0.1

### v2.0.0

* gcr.io/cloud-provider-vsphere/csi/release/driver:v2.0.0
* gcr.io/cloud-provider-vsphere/csi/release/syncer:v2.0.0

### v1.0.2

* gcr.io/cloud-provider-vsphere/csi/release/driver:v1.0.2
* gcr.io/cloud-provider-vsphere/csi/release/syncer:v1.0.2

### v1.0.1

* gcr.io/cloud-provider-vsphere/csi/release/driver:v1.0.1
* gcr.io/cloud-provider-vsphere/csi/release/syncer:v1.0.1

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on how to contribute.
