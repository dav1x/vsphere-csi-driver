/*
Copyright 2019 The Kubernetes Authors.

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

package wcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/pkg/syncer/podlistener"
)

const (
	defaultPodListenerServicePort = 10000
)

// ValidateCreateVolumeRequest is the helper function to validate
// CreateVolumeRequest for WCP CSI driver.
// Function returns error if validation fails otherwise returns nil.
func validateWCPCreateVolumeRequest(ctx context.Context, req *csi.CreateVolumeRequest) error {
	// Get create params
	params := req.GetParameters()
	for paramName := range params {
		paramName = strings.ToLower(paramName)
		if paramName != common.AttributeStoragePolicyID && paramName != common.AttributeFsType &&
			paramName != common.AttributeAffineToHost {
			msg := fmt.Sprintf("Volume parameter %s is not a valid WCP CSI parameter.", paramName)
			return status.Error(codes.InvalidArgument, msg)
		}
	}
	// Fail file volume creation
	if common.IsFileVolumeRequest(ctx, req.GetVolumeCapabilities()) {
		return status.Error(codes.InvalidArgument, "File volume not supported.")
	}
	return common.ValidateCreateVolumeRequest(ctx, req)
}

// validateWCPDeleteVolumeRequest is the helper function to validate
// DeleteVolumeRequest for WCP CSI driver.
// Function returns error if validation fails otherwise returns nil.
func validateWCPDeleteVolumeRequest(ctx context.Context, req *csi.DeleteVolumeRequest) error {
	return common.ValidateDeleteVolumeRequest(ctx, req)
}

// validateWCPControllerPublishVolumeRequest is the helper function to validate
// ControllerPublishVolumeRequest for WCP CSI driver. Function returns error if validation fails otherwise returns nil.
func validateWCPControllerPublishVolumeRequest(ctx context.Context, req *csi.ControllerPublishVolumeRequest) error {
	return common.ValidateControllerPublishVolumeRequest(ctx, req)
}

// validateWCPControllerUnpublishVolumeRequest is the helper function to validate
// ControllerUnpublishVolumeRequest for WCP CSI driver. Function returns error if validation fails otherwise returns nil.
func validateWCPControllerUnpublishVolumeRequest(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) error {
	return common.ValidateControllerUnpublishVolumeRequest(ctx, req)
}

// getVMUUIDFromPodListenerService gets the vmuuid from pod listener gRPC service
func getVMUUIDFromPodListenerService(ctx context.Context, volumeID string, nodeName string) (string, error) {
	var opts []grpc.DialOption
	log := logger.GetLogger(ctx)
	opts = append(opts, grpc.WithInsecure())
	port := getPodListenerServicePort(ctx)
	podListenerServiceAddr := "127.0.0.1:" + strconv.Itoa(port)
	// Connect to pod listerner gRPC service
	conn, err := grpc.Dial(podListenerServiceAddr, opts...)
	if err != nil {
		log.Errorf("failed to establish the connection to pod listener service when processing attach for volumeID: %s. Error: %+v", volumeID, err)
		return "", err
	}
	defer conn.Close()

	// Create a client stub for pod Listener gRPC service
	client := podlistener.NewPodListenerClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Call GetPodVMUUIDAnnotation method on the client stub
	res, err := client.GetPodVMUUIDAnnotation(ctx,
		&podlistener.Request{
			VolumeID: volumeID,
			NodeName: nodeName,
		})
	if err != nil {
		msg := fmt.Sprintf("failed to get the pod vmuuid annotation from the pod listener service. Error: %+v", err)
		log.Error(msg)
		return "", err
	}

	log.Infof("Got vmuuid: %s annotation from Pod Listener gRPC service", res.VmuuidAnnotation)
	return res.VmuuidAnnotation, nil
}

// getDatacenterFromConfig gets the vcenter-datacenter where WCP PodVM cluster is deployed
func getDatacenterFromConfig(cfg *config.Config) (map[string]string, error) {
	vcdcMap := make(map[string]string)
	vcdcListMap, err := getVCDatacentersFromConfig(cfg)
	if err != nil {
		return vcdcMap, err
	}
	if len(vcdcListMap) != 1 {
		return vcdcMap, fmt.Errorf("Found more than one vCenter instance. WCP Cluster can be deployed in only one VC")
	}

	for vcHost, dcList := range vcdcListMap {
		if len(dcList) > 1 {
			return vcdcMap, fmt.Errorf("Found more than one datacenter instances: %+v for vcHost: %s. WCP Cluster can be deployed in only one datacenter", dcList, vcHost)
		}
		vcdcMap[vcHost] = dcList[0]
	}
	return vcdcMap, nil
}

// GetVCDatacenters returns list of datacenters for each vCenter that is registered
func getVCDatacentersFromConfig(cfg *config.Config) (map[string][]string, error) {
	var err error
	vcdcMap := make(map[string][]string)
	for key, value := range cfg.VirtualCenter {
		dcList := strings.Split(value.Datacenters, ",")
		for _, dc := range dcList {
			dcMoID := strings.TrimSpace(dc)
			if dcMoID != "" {
				vcdcMap[key] = append(vcdcMap[key], dcMoID)
			}
		}
	}
	if len(vcdcMap) == 0 {
		err = errors.New("Unable get vCenter datacenters from vsphere config")
	}
	return vcdcMap, err
}

/*
 * getVMByInstanceUUIDInDatacenter gets the VM with the given instance UUID
 * in datacenter specified using datacenter moref value.
 */
func getVMByInstanceUUIDInDatacenter(ctx context.Context,
	vc *vsphere.VirtualCenter,
	datacenter string,
	vmInstanceUUID string) (*vsphere.VirtualMachine, error) {
	var dc *vsphere.Datacenter
	var vm *vsphere.VirtualMachine
	dc = &vsphere.Datacenter{
		Datacenter: object.NewDatacenter(vc.Client.Client,
			types.ManagedObjectReference{
				Type:  "Datacenter",
				Value: datacenter,
			}),
		VirtualCenterHost: vc.Config.Host,
	}
	// Get VM by UUID from datacenter
	vm, err := dc.GetVirtualMachineByUUID(ctx, vmInstanceUUID, true)
	if err != nil {
		return nil, fmt.Errorf("failed to the VM from the VM Instance UUID: %s in datacenter: %+v with err: %+v", vmInstanceUUID, dc, err)
	}
	return vm, nil
}

// getPodListenerServicePort return the port to connect the Pod Listener gRPC service.
// If environment variable POD_LISTENER_SERVICE_PORT is set and valid,
// return the interval value read from enviroment variable
// otherwise, use the default port
func getPodListenerServicePort(ctx context.Context) int {
	podListenerServicePort := defaultPodListenerServicePort
	log := logger.GetLogger(ctx)
	if v := os.Getenv("POD_LISTENER_SERVICE_PORT"); v != "" {
		if value, err := strconv.Atoi(v); err == nil {
			if value <= 0 {
				log.Warnf("Connecting to Pod Listener Service on port set in env variable POD_LISTENER_SERVICE_PORT %s is equal or less than 0, will use the default port %d", v, defaultPodListenerServicePort)
			} else {
				podListenerServicePort = value
				log.Infof("Connecting to Pod Listener Service on port %d", podListenerServicePort)
			}
		} else {
			log.Warnf("Connecting to Pod Listener Service on port set in env variable POD_LISTENER_SERVICE_PORT %s is invalid, will use the default port %d", v, defaultPodListenerServicePort)
		}
	}
	return podListenerServicePort
}
