/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/rexray/gocsi"

	csiconfig "sigs.k8s.io/vsphere-csi-driver/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/provider"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common/commonco"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/pkg/csi/types"
)

var (
	printVersion = flag.Bool("version", false, "Print driver version and exit")

	supervisorFSSName      = flag.String("supervisor-fss-name", "", "Name of the feature state switch configmap in supervisor cluster")
	supervisorFSSNamespace = flag.String("supervisor-fss-namespace", "", "Namespace of the feature state switch configmap in supervisor cluster")
	internalFSSName        = flag.String("fss-name", "", "Name of the feature state switch configmap")
	internalFSSNamespace   = flag.String("fss-namespace", "", "Namespace of the feature state switch configmap")
)

// main is ignored when this package is built as a go plug-in.
func main() {
	flag.Parse()
	if *printVersion {
		fmt.Printf("%s\n", service.Version)
		return
	}
	logType := logger.LogLevel(os.Getenv(logger.EnvLoggerLevel))
	logger.SetLoggerLevel(logType)
	ctx, log := logger.GetNewContextWithLogger()
	log.Infof("Version : %s", service.Version)

	// Set CO Init params
	clusterFlavor, err := csiconfig.GetClusterFlavor(ctx)
	if err != nil {
		log.Errorf("Failed retrieving cluster flavor. Error: %v", err)
	}
	commonco.SetInitParams(ctx, clusterFlavor, &service.COInitParams, *supervisorFSSName, *supervisorFSSNamespace,
		*internalFSSName, *internalFSSNamespace)

	gocsi.Run(
		context.Background(),
		csitypes.Name,
		"A CSI plugin for vSphere Cloud Native Storage",
		usage,
		provider.New())
}

const usage = `    VSPHERE_CSI_CONFIG
        Specifies the path to the csi-vsphere.conf file

        The default value is "/etc/cloud/csi-vsphere.conf"
`
