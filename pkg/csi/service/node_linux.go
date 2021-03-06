// +build linux
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
	"github.com/akutz/gofsutil"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
)

func (s *service) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("NodePublishVolume: called with args %+v", *req)
	var err error
	params := nodePublishParams{
		volID:  req.GetVolumeId(),
		target: req.GetTargetPath(),
		ro:     req.GetReadonly(),
	}
	// TODO: Verify if volume exists and return a NotFound error in negative scenario

	params.stagingTarget = req.GetStagingTargetPath()
	if params.stagingTarget == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "staging target path %q not set", params.stagingTarget)
	}

	// Check if this is a MountVolume or BlockVolume
	volCap := req.GetVolumeCapability()
	if !common.IsFileVolumeRequest(ctx, []*csi.VolumeCapability{volCap}) {
		params.diskID, err = getDiskID(req.GetPublishContext())
		if err != nil {
			log.Errorf("error fetching DiskID. Parameters: %v", params)
			return nil, err
		}

		log.Debugf("Checking if volume %q is attached to disk %q", params.volID, params.diskID)
		volPath, err := verifyVolumeAttached(ctx, params.diskID)
		if err != nil {
			log.Errorf("error checking if volume is attached. Parameters: %v", params)
			return nil, err
		}

		// Get underlying block device
		dev, err := getDevice(volPath)
		if err != nil {
			msg := fmt.Sprintf("error getting block device for volume: %q. Parameters: %v err: %v", params.volID, params, err)
			log.Error(msg)
			return nil, status.Errorf(codes.Internal, msg)
		}
		params.volumePath = dev.FullPath
		params.device = dev.RealDev

		// check for Block vs Mount
		if _, ok := volCap.GetAccessType().(*csi.VolumeCapability_Block); ok {
			// bind mount device to target
			return publishBlockVol(ctx, req, dev, params)
		}
		// Volume must be a mount volume
		return publishMountVol(ctx, req, dev, params)
	}
	// Volume must be a file share
	return publishFileVol(ctx, req, params)
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

	// Verify if the path exists
	// NOTE: For raw block volumes, this path is a file. In all other cases, it is a directory
	_, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			// target path does not exist, so we must be Unpublished
			log.Infof("NodeUnpublishVolume: Target path %q does not exist. Assuming NodeUnpublish is complete", target)
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"failed to stat target %q, err: %v", target, err)
	}

	// Fetch all the mount points
	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not retrieve existing mount points: %q",
			err.Error())
	}
	log.Debugf("NodeUnpublishVolume: node mounts %+v", mnts)

	// Check if the volume is already unpublished
	// Also validates if path is a mounted directory or not
	isPresent := common.IsTargetInMounts(ctx, target, mnts)
	if !isPresent {
		log.Infof("NodeUnpublishVolume: Target %s not present in mount points. Assuming it is already unpublished.", target)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	// Figure out if the target path is a file or block volume
	isFileMount, _ := common.IsFileVolumeMount(ctx, target, mnts)
	isPublished := true
	if !isFileMount {
		isPublished, err = isBlockVolumePublished(ctx, volID, target)
		if err != nil {
			return nil, err
		}
	}

	if isPublished {
		log.Infof("NodeUnpublishVolume: Attempting to unmount target %q for volume %q", target, volID)
		if err := gofsutil.Unmount(ctx, target); err != nil {
			msg := fmt.Sprintf("Error unmounting target %q for volume %q. %q", target, volID, err.Error())
			log.Debug(msg)
			return nil, status.Error(codes.Internal, msg)
		}
		log.Debugf("Unmount successful for target %q for volume %q", target, volID)
		// TODO Use a go routine here. The deletion of target path might not be a good reason to error out
		// The SP is supposed to delete the files/directory it created in this target path
		if err := rmpath(ctx, target); err != nil {
			log.Debugf("failed to delete the target path %q", target)
			return nil, err
		}
		log.Debugf("Target path  %q successfully deleted", target)
	}
	log.Infof("NodeUnpublishVolume successful for volume %q", volID)

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
	// Fetch all the mount points
	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not retrieve existing mount points: %v", err)
	}
	log.Debugf("NodeUnstageVolume: node mounts %+v", mnts)
	// Figure out if the target path is present in mounts or not - Unstage is not required for file volumes
	targetFound := common.IsTargetInMounts(ctx, stagingTarget, mnts)
	if !targetFound {
		log.Infof("NodeUnstageVolume: Target path %q is not mounted. Skipping unstage.", stagingTarget)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	volID := req.GetVolumeId()
	dirExists, err := verifyTargetDir(ctx, stagingTarget, false)
	if err != nil {
		return nil, err
	}
	// This will take care of idempotent requests
	if !dirExists {
		log.Infof("NodeUnstageVolume: Target path %q does not exist. Assuming unstage is complete.", stagingTarget)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	// Block volume
	isMounted, err := isBlockVolumeMounted(ctx, volID, stagingTarget)
	if err != nil {
		return nil, err
	}

	// Volume is still mounted. Unstage the volume
	if isMounted {
		log.Infof("Attempting to unmount target %q for volume %q", stagingTarget, volID)
		if err := gofsutil.Unmount(ctx, stagingTarget); err != nil {
			return nil, status.Errorf(codes.Internal,
				"Error unmounting stagingTarget: %v", err)
		}
	}
	log.Infof("NodeUnstageVolume successful for target %q for volume %q", stagingTarget, volID)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

//isBlockVolumeMounted checks if the block volume is properly mounted or not.
//If yes, then the calling function proceeds to unmount the volume
func isBlockVolumeMounted(
	ctx context.Context,
	volID string,
	stagingTargetPath string) (
	bool, error) {

	log := logger.GetLogger(ctx)
	// Look up block device mounted to target
	// BlockVolume: Here we are relying on the fact that the CO is required to
	// have created the staging path per the spec, even for BlockVolumes. Even
	// though we don't use the staging path for block, the fact nothing will be
	// mounted still indicates that unstaging is done.
	dev, err := getDevFromMount(stagingTargetPath)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"isBlockVolumeMounted: error getting block device for volume: %s, err: %s",
			volID, err.Error())
	}

	// No device found
	if dev == nil {
		// Nothing is mounted, so unstaging is already done
		log.Debugf("isBlockVolumeMounted: No device found. Assuming Unstage is "+
			"already done for volume %q and target path %q", volID, stagingTargetPath)
		// For raw block volumes, we do not get a device attached to the stagingTargetPath. This path is just a dummy.
		return false, nil
	}
	log.Debugf("found device: volID: %q, path: %q, block: %q, target: %q", volID, dev.FullPath, dev.RealDev, stagingTargetPath)

	// Get mounts for device
	mnts, err := gofsutil.GetDevMounts(ctx, dev.RealDev)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"isBlockVolumeMounted: could not reliably determine existing mount status: %s",
			err.Error())
	}

	// device is mounted more than once. Should only be mounted to target
	if len(mnts) > 1 {
		return false, status.Errorf(codes.Internal,
			"isBlockVolumeMounted: volume: %s appears mounted in multiple places", volID)
	}

	// Since we looked up the block volume from the target path, we assume that
	// the one existing mount is from the block to the target
	log.Debugf("isBlockVolumeMounted: Found single mount point for volume %q and target %q",
		volID, stagingTargetPath)
	return true, nil
}

// isBlockVolumePublished checks if the device backing block volume exists.
func isBlockVolumePublished(ctx context.Context, volID string, target string) (bool, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)

	// Look up block device mounted to target
	dev, err := getDevFromMount(target)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"error getting block device for volume: %s, err: %v",
			volID, err)
	}

	if dev == nil {
		// Nothing is mounted, so unpublish is already done. However, we also know
		// that the target path exists, and it is our job to remove it.
		log.Debugf("isBlockVolumePublished: No device found. Assuming Unpublish is "+
			"already complete for volume %q and target path %q", volID, target)
		if err := rmpath(ctx, target); err != nil {
			msg := fmt.Sprintf("Failed to delete the target path %q. Error: %v", target, err)
			log.Debug(msg)
			return false, status.Errorf(codes.Internal, msg)
		}
		log.Debugf("isBlockVolumePublished: Target path %q successfully deleted", target)
		return false, nil
	}
	return true, nil
}

func nodeStageBlockVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
	params nodeStageParams) (
	*csi.NodeStageVolumeResponse, error) {
	log := logger.GetLogger(ctx)
	pubCtx := req.GetPublishContext()
	diskID, err := getDiskID(pubCtx)
	// Block Volume
	if err != nil {
		return nil, err
	}
	log.Infof("NodeStageVolume: volID %q, published context %+v, diskID %q",
		params.volID, pubCtx, diskID)

	// Verify if the volume is attached

	log.Debugf("NodeStageVolume: Checking if volume is attached with diskID: %v", diskID)
	volPath, err := verifyVolumeAttached(ctx, diskID)
	if err != nil {
		return nil, err
	}
	log.Debugf("NodeStageVolume: Disk %q attached at %q", diskID, volPath)

	// Check that block device looks good
	dev, err := getDevice(volPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"error getting block device for volume: %q, err: %q",
			params.volID, err.Error())
	}
	log.Debugf("NodeStageVolume: getDevice %+v", *dev)

	// Check if this is a MountVolume or BlockVolume
	if _, ok := req.GetVolumeCapability().GetAccessType().(*csi.VolumeCapability_Block); ok {
		// Volume is a block volume, so skip the rest of the steps
		log.Infof("skipping staging for block access type")
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Mount Volume
	// Fetch dev mounts to check if the device is already staged
	log.Debugf("NodeStageVolume: Fetching device mounts")
	mnts, err := gofsutil.GetDevMounts(ctx, dev.RealDev)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not reliably determine existing mount status: %q",
			err.Error())
	}

	if len(mnts) == 0 {
		// Device isn't mounted anywhere, stage the volume
		// If access mode is read-only, we don't allow formatting
		if params.ro {
			log.Debugf("NodeStageVolume: Mounting %q at %q in read-only mode with mount flags %v",
				dev.FullPath, params.stagingTarget, params.mntFlags)
			params.mntFlags = append(params.mntFlags, "ro")
			if err := gofsutil.Mount(ctx, dev.FullPath, params.stagingTarget, params.fsType, params.mntFlags...); err != nil {
				return nil, status.Errorf(codes.Internal,
					"error with mount during staging: %q",
					err.Error())
			}
			log.Debugf("NodeStageVolume: Device mounted successfully at %q", params.stagingTarget)
			return &csi.NodeStageVolumeResponse{}, nil
		}
		// Format and mount the device
		log.Debugf("NodeStageVolume: Format and mount the device %q at %q with mount flags %v",
			dev.FullPath, params.stagingTarget, params.mntFlags)
		if err := gofsutil.FormatAndMount(ctx, dev.FullPath, params.stagingTarget, params.fsType, params.mntFlags...); err != nil {
			return nil, status.Errorf(codes.Internal,
				"error with format and mount during staging: %q",
				err.Error())
		}
		log.Debugf("NodeStageVolume: Device mounted successfully at %q", params.stagingTarget)
		return &csi.NodeStageVolumeResponse{}, nil
	}
	// If Device is already mounted. Need to ensure that it is already
	// mounted to the expected staging target, with correct rw/ro perms
	log.Debugf("NodeStageVolume: Device already mounted. Checking mount flags %v for correctness.",
		params.mntFlags)
	mounted := false
	for _, m := range mnts {
		if m.Path == params.stagingTarget {
			mounted = true
			rwo := "rw"
			if params.ro {
				rwo = "ro"
			}
			log.Debugf("NodeStageVolume: Checking for mount options %v", m.Opts)
			if contains(m.Opts, rwo) {
				//TODO make sure that all the mount options match
				log.Debugf("NodeStageVolume: Device already mounted at %q with mount option %q",
					params.stagingTarget, rwo)
				return &csi.NodeStageVolumeResponse{}, nil
			}
			return nil, status.Error(codes.AlreadyExists,
				"access mode conflicts with existing mount")
		}
	}
	if !mounted {
		return nil, status.Error(codes.Internal,
			"device already in use and mounted elsewhere")
	}
	return nil, nil
}

// a wrapper around gofsutil.GetMounts that handles bind mounts
func getDevMounts(ctx context.Context,
	sysDevice *Device) ([]gofsutil.Info, error) {

	devMnts := make([]gofsutil.Info, 0)

	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return devMnts, err
	}
	for _, m := range mnts {
		if m.Device == sysDevice.RealDev || (m.Device == "devtmpfs" && m.Source == sysDevice.RealDev) {
			devMnts = append(devMnts, m)
		}
	}
	return devMnts, nil
}

func publishMountVol(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
	dev *Device,
	params nodePublishParams) (
	*csi.NodePublishVolumeResponse, error) {
	log := logger.GetLogger(ctx)
	log.Infof("PublishMountVolume called with args: %+v", params)

	// Extract fs details
	_, mntFlags, err := ensureMountVol(ctx, req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}

	// We are responsible for creating target dir, per spec, if not already present
	_, err = mkdir(ctx, params.target)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"Unable to create target dir: %q, err: %v", params.target, err)
	}
	log.Debugf("PublishMountVolume: Created target path %q", params.target)

	// Verify if the Staging path already exists
	if _, err := verifyTargetDir(ctx, params.stagingTarget, true); err != nil {
		return nil, err
	}

	// get block device mounts
	// Check if device is already mounted
	devMnts, err := getDevMounts(ctx, dev)
	if err != nil {
		msg := fmt.Sprintf("could not reliably determine existing mount status. Parameters: %v err: %v", params, err)
		log.Error(msg)
		return nil, status.Error(codes.Internal, msg)
	}
	log.Debugf("publishMountVol: device %+v, device mounts %q", *dev, devMnts)

	// We expect that block device is already staged, so there should be at least 1
	// mount already. if it's > 1, it may already be published
	if len(devMnts) > 1 {
		// check if publish is already there
		for _, m := range devMnts {
			if m.Path == params.target {
				// volume already published to target
				// if mount options look good, do nothing
				rwo := "rw"
				if params.ro {
					rwo = "ro"
				}
				if !contains(m.Opts, rwo) {
					//TODO make sure that all the mount options match
					return nil, status.Error(codes.AlreadyExists,
						"volume previously published with different options")
				}

				// Existing mount satisfies request
				log.Infof("Volume already published to target. Parameters: [%+v]", params)
				return &csi.NodePublishVolumeResponse{}, nil
			}
		}
	} else if len(devMnts) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Volume ID: %q does not appear staged to %q", req.GetVolumeId(), params.stagingTarget)
	}

	// Do the bind mount to publish the volume
	if params.ro {
		mntFlags = append(mntFlags, "ro")
	}
	log.Debugf("PublishMountVolume: Attempting to bind mount %q to %q with mount flags %v",
		params.stagingTarget, params.target, mntFlags)
	if err := gofsutil.BindMount(ctx, params.stagingTarget, params.target, mntFlags...); err != nil {
		msg := fmt.Sprintf("error mounting volume. Parameters: %v err: %v", params, err)
		log.Error(msg)
		return nil, status.Error(codes.Internal, msg)
	}
	log.Infof("NodePublishVolume for %q successful to path %q", req.GetVolumeId(), params.target)
	return &csi.NodePublishVolumeResponse{}, nil
}

func publishBlockVol(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
	dev *Device,
	params nodePublishParams) (
	*csi.NodePublishVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("PublishBlockVolume called with args: %+v", params)

	// We are responsible for creating target file, per spec, if not already present
	_, err := mkfile(ctx, params.target)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"Unable to create target file: %q, err: %v", params.target, err)
	}
	log.Debugf("publishBlockVol: Target %q created", params.target)

	// Read-only is not supported for BlockVolume. Doing a read-only
	// bind mount of the device to the target path does not prevent
	// the underlying block device from being modified, so don't
	// advertise a false sense of security
	if params.ro {
		return nil, status.Error(codes.InvalidArgument,
			"read only not supported for Block Volume")
	}

	// get block device mounts
	devMnts, err := getDevMounts(ctx, dev)
	if err != nil {
		msg := fmt.Sprintf("could not reliably determine existing mount status. Parameters: %v err: %v", params, err)
		log.Error(msg)
		return nil, status.Error(codes.Internal, msg)
	}
	log.Debugf("publishBlockVol: device %+v, device mounts %q", *dev, devMnts)

	// check if device is already mounted
	if len(devMnts) == 0 {
		// do the bind mount
		mntFlags := make([]string, 0)
		log.Debugf("PublishBlockVolume: Attempting to bind mount %q to %q with mount flags %v",
			dev.FullPath, params.target, mntFlags)
		if err := gofsutil.BindMount(ctx, dev.FullPath, params.target, mntFlags...); err != nil {
			msg := fmt.Sprintf("error mounting volume. Parameters: %v err: %v", params, err)
			log.Error(msg)
			return nil, status.Error(codes.Internal, msg)
		}
		log.Debugf("PublishBlockVolume: Bind mount successful to path %q", params.target)
	} else if len(devMnts) == 1 {
		// already mounted, make sure it's what we want
		if devMnts[0].Path != params.target {
			return nil, status.Error(codes.Internal,
				"device already in use and mounted elsewhere")
		}
		log.Debugf("Volume already published to target. Parameters: [%+v]", params)
	} else {
		return nil, status.Error(codes.AlreadyExists,
			"block volume already mounted in more than one place")
	}
	log.Infof("NodePublishVolume successful to path %q", params.target)

	// existing or new mount satisfies request
	return &csi.NodePublishVolumeResponse{}, nil
}

func publishFileVol(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
	params nodePublishParams) (
	*csi.NodePublishVolumeResponse, error) {
	log := logger.GetLogger(ctx)
	log.Infof("PublishFileVolume called with args: %+v", params)

	// Extract mount details
	fsType, mntFlags, err := ensureMountVol(ctx, req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}

	// We are responsible for creating target dir, per spec, if not already present
	_, err = mkdir(ctx, params.target)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"Unable to create target dir: %q, err: %v", params.target, err)
	}
	log.Debugf("PublishFileVolume: Created target path %q", params.target)

	// Check if target already mounted
	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not retrieve existing mount points: %q",
			err.Error())
	}
	log.Debugf("PublishFileVolume: Mounts - %+v", mnts)
	for _, m := range mnts {
		if m.Path == params.target {
			// volume already published to target
			// if mount options look good, do nothing
			rwo := "rw"
			if params.ro {
				rwo = "ro"
			}
			if !contains(m.Opts, rwo) {
				//TODO make sure that all the mount options match
				return nil, status.Error(codes.AlreadyExists,
					"volume previously published with different options")
			}

			// Existing mount satisfies request
			log.Infof("Volume already published to target %q.", params.target)
			return &csi.NodePublishVolumeResponse{}, nil
		}
	}

	// Check for read-only flag on Pod pvc spec
	if params.ro {
		mntFlags = append(mntFlags, "ro")
	}
	// Retrieve the file share access point from publish context
	mntSrc, ok := req.GetPublishContext()[common.Nfsv4AccessPoint]
	if !ok {
		return nil, status.Error(codes.Internal, "NFSv4 accesspoint not set in publish context")
	}
	// Directly mount the file share volume to the pod. No bind mount required.
	log.Debugf("PublishFileVolume: Attempting to mount %q to %q with fstype %q and mountflags %v",
		mntSrc, params.target, fsType, mntFlags)
	if err := gofsutil.Mount(ctx, mntSrc, params.target, fsType, mntFlags...); err != nil {
		return nil, status.Errorf(codes.Internal,
			"error publish volume to target path: %q",
			err.Error())
	}
	log.Infof("NodePublishVolume successful to path %q", params.target)

	return &csi.NodePublishVolumeResponse{}, nil
}