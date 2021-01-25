// +build windows

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

package service

import (
	"context"
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/davecgh/go-spew/spew"
	diskapi "github.com/kubernetes-csi/csi-proxy/client/api/disk/v1beta2"
	diskclient "github.com/kubernetes-csi/csi-proxy/client/groups/disk/v1beta2"
	filesystemapi "github.com/kubernetes-csi/csi-proxy/client/api/filesystem/v1beta1"
	filesystemclient "github.com/kubernetes-csi/csi-proxy/client/groups/filesystem/v1beta1"
	volumeapi "github.com/kubernetes-csi/csi-proxy/client/api/volume/v1beta2"
	volumeclient "github.com/kubernetes-csi/csi-proxy/client/groups/volume/v1beta2"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/utils/mount"
	"os"
	"path/filepath"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	"strconv"
)

func (s *service) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse, error) {

	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("NodePublishVolume: called with args %+v", *req)
	params := nodePublishParams{
		volID: req.GetVolumeId(),
		target: req.GetTargetPath(),
		stagingTarget: req.GetStagingTargetPath(),
	}

	if params.stagingTarget == "" {
		return nil, status.Error(codes.FailedPrecondition, "staging target path not set")
	}
	if params.target == "" {
		return nil, status.Error(codes.FailedPrecondition, "target path not set")
	}


	isMnt, err := isMountPoint(ctx, params.target); if err != nil {
		return nil, errors.Wrap(err, "unable to verify mount point")
	}
	if isMnt {
		log.Infof("volume %s is already mounted at path %s", params.volID, params.target)
	}
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.FailedPrecondition, "volume capability not set")
	}

	// Check if mount volume
	if _, ok := volCap.GetAccessType().(*csi.VolumeCapability_Mount); ok {
		if err := publishMountVol(ctx, params); err != nil {
			// TODO: wrap error returned
			log.Error(err)
			return nil, status.Error(codes.Internal, "unable to publish mount volume")
		}
	} else {
		return nil, status.Error(codes.Unimplemented, "volume types other than mount unimplemented")
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *service) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest) (
	*csi.NodeUnpublishVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("NodeUnpublishVolume: called with args %+v", *req)

	volID := req.GetVolumeId()
	target := req.GetTargetPath()

	if volID == "" {
		return nil, status.Error(codes.FailedPrecondition, "volume ID not set")
	}
	if target == "" {
		return nil, status.Error(codes.FailedPrecondition, "target path not set")
	}

	filesystemClient, err := filesystemclient.NewClient()
	if err != nil {
		return nil, err
	}

	rmdirRequest := &filesystemapi.RmdirRequest{
		Path: mount.NormalizeWindowsPath(target),
		Context: filesystemapi.PathContext_POD,
		Force:   true,
	}
	_, err = filesystemClient.Rmdir(context.Background(), rmdirRequest)

	if err != nil {
		return nil, err
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *service) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest) (
	*csi.NodeUnstageVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("NodeUnstageVolume: called with args %+v", *req)

	stagingTarget := req.GetStagingTargetPath()

	err := dismountVolume(ctx, stagingTarget)
	if err != nil {
		return nil, errors.Wrap(err, "unable to dismount volume")
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func dismountVolume(ctx context.Context, target string) error {
	fsClient, err := filesystemclient.NewClient()
	if err != nil {
		return err
	}
	volumeClient, err := volumeclient.NewClient()
	if err != nil {
		return err
	}
	diskClient, err := diskclient.NewClient()
	if err != nil {
		return err
	}
	log := logger.GetLogger(ctx)
	path := mount.NormalizeWindowsPath(target)
	//	check path exists
	fsPathReq := &filesystemapi.PathExistsRequest{Path: path}
	fsPathResp, err := fsClient.PathExists(context.Background(), fsPathReq)
	if err != nil {
		return err
	}
	if !fsPathResp.Exists {
		return nil
	}
	// csiproxy get volumeID from path
	volIDReq := &volumeapi.VolumeIDFromMountRequest{Mount: target}
	volIDResp, err := volumeClient.GetVolumeIDFromMount(context.Background(), volIDReq)
	if err != nil {
		return err
	}
	volumeID := volIDResp.VolumeId

	// csiproxy dismount volume
	dismountReq := &volumeapi.DismountVolumeRequest{
		Path: path,
		VolumeId: volumeID,
	}
	_, err = volumeClient.DismountVolume(context.Background(), dismountReq)
	if err != nil {
		return err
	}

	// csiproxy rmdir
	rmDirReq := &filesystemapi.RmdirRequest{
		Path: path,
		Context: filesystemapi.PathContext_PLUGIN,
		Force: true,
	}
	_, err = fsClient.Rmdir(context.Background(), rmDirReq)
	if err != nil {
		return err
	}

	// csiproxy getdisknumber
	getDiskNumberReq := &volumeapi.VolumeDiskNumberRequest{
		VolumeId: volumeID,
	}
	getDiskNumberResp, err := volumeClient.GetVolumeDiskNumber(context.Background(), getDiskNumberReq)
	if err != nil {
		return err
	}
	// csiproxy setattachstate
	log.Infof("setting disk number %d to offline", getDiskNumberResp.DiskNumber)
	attachStateReq := &diskapi.SetAttachStateRequest{
		DiskID: strconv.FormatInt(getDiskNumberResp.DiskNumber, 10),
		IsOnline: false,
	}
	_, err = diskClient.SetAttachState(context.Background(), attachStateReq)
	if err != nil {
		return err
	}

	return nil
}

func publishMountVol(ctx context.Context, params nodePublishParams) error {
	filesystemClient, err := filesystemclient.NewClient()
	if err != nil {
		return err
	}
	// delete target path if it exists
	rmdirRequest := &filesystemapi.RmdirRequest{
		Path:    mount.NormalizeWindowsPath(params.target),
		Context: filesystemapi.PathContext_POD,
		Force:   true,
	}
	_, err = filesystemClient.Rmdir(context.Background(), rmdirRequest)
	if err != nil {
		return err
	}

	// create target's parent dir if it doesn't exist
	parentDir := filepath.Dir(params.target)
	if err := os.MkdirAll(parentDir, 0777); err != nil {
		return err
	}

	// mount path using csi-proxy link path
	linkRequest := &filesystemapi.LinkPathRequest{
		SourcePath: mount.NormalizeWindowsPath(params.stagingTarget),
		TargetPath: mount.NormalizeWindowsPath(params.target),
	}
	linkResponse, err := filesystemClient.LinkPath(context.Background(), linkRequest)
	if err != nil {
		return err
	}
	if linkResponse.GetError() != "" {
		return errors.New(linkResponse.GetError())
	}

	return nil
}

func nodeStageBlockVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
	params nodeStageParams) (
	*csi.NodeStageVolumeResponse, error) {

	// TODO: make reusable csi-proxy clients
	// TODO: wrap errors into grpc errors
	log := logger.GetLogger(ctx)
	pubCtx := req.GetPublishContext()
	diskID, err := getDiskID(pubCtx)
	stagingTargetPath := req.GetStagingTargetPath()
	if err != nil {
		return nil, err
	}

	diskNumber, err := getDiskNumber(ctx, diskID)
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("unable to find attached disk with ID: %s", diskID))
	}

	log.Infof("Found disk number: %s", diskNumber)

	// return early if block type
	if _, ok := req.GetVolumeCapability().GetAccessType().(*csi.VolumeCapability_Block); ok {
		// Volume is a block volume, so skip the rest of the steps
		log.Infof("skipping staging for block access type")
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// check if path is already mounted
	isMounted, err := isMountPoint(ctx, stagingTargetPath)
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("unable to verify mount point: %s", stagingTargetPath))
	}
	if isMounted {
		log.Info("volume is already successfully mounted")
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// format and mount new volume
	err = formatAndMount(ctx, stagingTargetPath, diskNumber)
	if err != nil {
		return nil, errors.Wrap(err, "unable to mount volume")
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func formatAndMount(ctx context.Context, path string, diskNum string) error {
	// TODO: handle Read-ONLY where we don't format the volume
	log := logger.GetLogger(ctx)
	diskClient, err := diskclient.NewClient()
	if err != nil {
		return err
	}
	volumeClient, err := volumeclient.NewClient()
	if err != nil {
		return err
	}

	// partition disk
	log.Infof("partitioning disk %s", diskNum)
	partitionRequest := &diskapi.PartitionDiskRequest{DiskID: diskNum}
	_, err = diskClient.PartitionDisk(context.Background(), partitionRequest)
	if err != nil {
		return err
	}

	// ensure disk is online
	log.Infof("setting disk %s to online", diskNum)
	attachRequest := &diskapi.SetAttachStateRequest{
		DiskID:   diskNum,
		IsOnline: true,
	}
	_, err = diskClient.SetAttachState(context.Background(), attachRequest)
	if err != nil {
		return err
	}

	// get volume
	volumesRequest := &volumeapi.ListVolumesOnDiskRequest{DiskId: diskNum}
	volumesResponse, err := volumeClient.ListVolumesOnDisk(context.Background(), volumesRequest)
	if err != nil {
		return err
	}

	if len(volumesResponse.GetVolumeIds()) == 0 {
		return errors.New("no volumes found on disk")
	}

	volID := volumesResponse.GetVolumeIds()[0]
	// check if volume is formatted
	volumeFormattedRequest := &volumeapi.IsVolumeFormattedRequest{VolumeId: volID}
	volumeFormattedResponse, err := volumeClient.IsVolumeFormatted(context.Background(), volumeFormattedRequest)
	if err != nil {
		return err
	}
	if !volumeFormattedResponse.Formatted {
		// format volume
		log.Infof("formatting volume")
		formatVolumeRequest := &volumeapi.FormatVolumeRequest{VolumeId: volID}
		_, err = volumeClient.FormatVolume(context.Background(), formatVolumeRequest)
		if err != nil {
			return err
		}
	}

	// mount volume
	mountVolumeRequest := &volumeapi.MountVolumeRequest{
		VolumeId: volID,
		Path: path,
	}
	_, err = volumeClient.MountVolume(context.Background(), mountVolumeRequest)
	if err != nil {
		return err
	}
	log.Info("volume formatted and mounted")

	return nil
}

func isMountPoint(ctx context.Context, path string) (bool, error) {
	filesystemClient, err := filesystemclient.NewClient()
	if err != nil {
		return false, err
	}
	isMountRequest := &filesystemapi.IsMountPointRequest{
		Path: path,
	}
	isMountResponse, err := filesystemClient.IsMountPoint(context.Background(), isMountRequest)
	if err != nil {
		return false, err
	}

	return isMountResponse.IsMountPoint, nil
}

func getDiskNumber(ctx context.Context, diskID string) (string, error) {
	log := logger.GetLogger(ctx)
	// Check device is attached
	diskClient, err := diskclient.NewClient()
	if err != nil {
		return "", err
	}
	listRequest := &diskapi.ListDiskIDsRequest{}
	diskIDsResponse, err := diskClient.ListDiskIDs(context.Background(), listRequest)
	if err != nil {
		return "", err
	}
	spew.Dump("disIDs: ", diskIDsResponse)

	for diskNum, diskInfo := range diskIDsResponse.GetDiskIDs() {
		ID, ok := diskInfo.Identifiers["page83"]
		if !ok || ID == "" {
			continue
		}

		if ID == diskID {
			log.Infof("Found disk number: %s with diskID: %s", diskNum, diskID)
			return diskNum, nil
		}
	}

	return "", errors.New("no matching disks found")
}
