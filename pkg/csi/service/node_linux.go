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