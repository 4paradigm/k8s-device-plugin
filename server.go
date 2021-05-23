/*
 * Copyright (c) 2019, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/go-gpuallocator/gpuallocator"
	"github.com/google/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Constants to represent the various device list strategies
const (
	DeviceListStrategyEnvvar       = "envvar"
	DeviceListStrategyVolumeMounts = "volume-mounts"
)

// Constants to represent the various device id strategies
const (
	DeviceIDStrategyUUID  = "uuid"
	DeviceIDStrategyIndex = "index"
)

// Constants for use by the 'volume-mounts' device list strategy
const (
	deviceListAsVolumeMountsHostPath          = "/dev/null"
	deviceListAsVolumeMountsContainerPathRoot = "/var/run/nvidia-container-devices"
)

// NvidiaDevicePlugin implements the Kubernetes device plugin API
type NvidiaDevicePlugin struct {
	ResourceManager
	resourceName     string
	deviceListEnvvar string
	allocatePolicy   gpuallocator.Policy
	socket           string

	server        *grpc.Server
	cachedDevices []*Device
	health        chan *Device
	stop          chan interface{}
	vDevices      []*VDevice
	vidmap        map[string]int
	vidoccupy     map[string]int
	remaining     map[string]int
	allocid       int
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewNvidiaDevicePlugin(resourceName string, resourceManager ResourceManager, deviceListEnvvar string, allocatePolicy gpuallocator.Policy, socket string) *NvidiaDevicePlugin {
	return &NvidiaDevicePlugin{
		ResourceManager:  resourceManager,
		resourceName:     resourceName,
		deviceListEnvvar: deviceListEnvvar,
		allocatePolicy:   allocatePolicy,
		socket:           socket,

		// These will be reinitialized every
		// time the plugin server is restarted.
		cachedDevices: nil,
		server:        nil,
		health:        nil,
		stop:          nil,
		vidmap:        make(map[string]int),
		vidoccupy:     make(map[string]int),
		remaining:     make(map[string]int),
		allocid:       0,
	}
}

func (m *NvidiaDevicePlugin) initialize() {
	m.cachedDevices = m.Devices()
	m.vDevices = Device2VDevice(m.cachedDevices)
	for i, val := range m.vDevices {
		m.vidmap[val.Device.ID] = i
		m.vidoccupy[val.Device.ID] = -1
		m.remaining[UniqueDeviceIDs([]*VDevice{val})[0]]++
	}
	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	m.health = make(chan *Device)
	m.stop = make(chan interface{})
	m.allocid = 0
}

func (m *NvidiaDevicePlugin) cleanup() {
	close(m.stop)
	m.vDevices = nil
	m.cachedDevices = nil
	m.server = nil
	m.health = nil
	m.stop = nil
}

func getNextId(m *NvidiaDevicePlugin) int {
	t := m.allocid
	m.allocid++
	if m.allocid == len(m.vDevices) {
		m.allocid = 0
	}
	return t
}

func getMaxRemaining(m *NvidiaDevicePlugin) string {
	maxr := 0
	pos := ""
	for idx, val := range m.remaining {
		if val > maxr && val > 0 {
			pos = idx
			maxr = val
		}
	}
	if maxr > 0 {
		fmt.Println("reducing.... ", pos)
		m.remaining[pos]--
	}
	return pos
}

func addOccupancy(m *NvidiaDevicePlugin, vdev []*VDevice) error {
	occupyid := getNextId(m)
	for _, val := range vdev {
		m.vidoccupy[val.Device.ID] = occupyid
	}
	return nil
}

func allocateAndGet(m *NvidiaDevicePlugin, count int) []string {
	resp := []string{}
	fmt.Println("into allocateAndGet,count=", count)
	for i := 0; i < count; i++ {
		tmp := getMaxRemaining(m)
		if tmp != "" {
			resp = append(resp, tmp)
		} else {
			printLogs(m)
			panic(1)
		}
	}
	return resp
}

func printLogs(m *NvidiaDevicePlugin) {
	fmt.Println("AllocateAndGet error")
	fmt.Println("remaining=", m.remaining, ": occupancy=", m.vidoccupy)
}

func freeOccupyId(m *NvidiaDevicePlugin, id int) error {
	found := false
	for idx, val := range m.vidoccupy {
		if val == id {
			m.vidoccupy[idx] = -1
			vdx, _ := VDevicesByIDs(m.vDevices, []string{idx})
			m.remaining[UniqueDeviceIDs(vdx)[0]]++
			fmt.Println("Freeed", idx)
			found = true
		}
	}
	if !found {
		printLogs(m)
		panic(1)
	}
	return nil
}

func freeOccupy(m *NvidiaDevicePlugin, vdev []*VDevice) error {
	for _, val := range vdev {
		if m.vidoccupy[val.Device.ID] != -1 {
			freeOccupyId(m, m.vidoccupy[val.Device.ID])
		} else {
			fmt.Println("Is -1 skipping")
		}
	}
	return nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (m *NvidiaDevicePlugin) Start() error {
	m.initialize()

	err := m.Serve()
	if err != nil {
		log.Printf("Could not start device plugin for '%s': %s", m.resourceName, err)
		m.cleanup()
		return err
	}
	log.Printf("Starting to serve '%s' on %s", m.resourceName, m.socket)

	err = m.Register()
	if err != nil {
		log.Printf("Could not register device plugin: %s", err)
		m.Stop()
		return err
	}
	log.Printf("Registered device plugin for '%s' with Kubelet", m.resourceName)

	go m.CheckHealth(m.stop, m.cachedDevices, m.health)

	return nil
}

// Stop stops the gRPC server.
func (m *NvidiaDevicePlugin) Stop() error {
	if m == nil || m.server == nil {
		return nil
	}
	log.Printf("Stopping to serve '%s' on %s", m.resourceName, m.socket)
	m.server.Stop()
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.cleanup()
	return nil
}

// Serve starts the gRPC server of the device plugin.
func (m *NvidiaDevicePlugin) Serve() error {
	os.Remove(m.socket)
	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			log.Printf("Starting GRPC server for '%s'", m.resourceName)
			err := m.server.Serve(sock)
			if err == nil {
				break
			}

			log.Printf("GRPC server for '%s' crashed with error: %v", m.resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				log.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", m.resourceName)
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := m.dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *NvidiaDevicePlugin) Register() error {
	conn, err := m.dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: m.resourceName,
		Options: &pluginapi.DevicePluginOptions{
			GetPreferredAllocationAvailable: m.allocatePolicy != nil,
		},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// GetDevicePluginOptions returns the values of the optional settings for this plugin
func (m *NvidiaDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	options := &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: m.allocatePolicy != nil,
	}
	return options, nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *NvidiaDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: m.apiDevices()})

	for {
		select {
		case <-m.stop:
			return nil
		case d := <-m.health:
			// FIXME: there is no way to recover from the Unhealthy state.
			d.Health = pluginapi.Unhealthy
			log.Printf("'%s' device marked unhealthy: %s", m.resourceName, d.ID)
			s.Send(&pluginapi.ListAndWatchResponse{Devices: m.apiDevices()})
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request
func (m *NvidiaDevicePlugin) GetPreferredAllocation(ctx context.Context, r *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {

	response := &pluginapi.PreferredAllocationResponse{}
	// get device
	fmt.Println("GetPreferredAllocation")
	for _, req := range r.ContainerRequests {
		availableVDev, err := VDevicesByIDs(m.vDevices, req.AvailableDeviceIDs)
		if err != nil {
			return nil, fmt.Errorf("Unable to retrieve list of available vdevices: %v", err)
		}
		available, err := gpuallocator.NewDevicesFrom(UniqueDeviceIDs(availableVDev))
		if err != nil {
			return nil, fmt.Errorf("Unable to retrieve list of available devices: %v", err)
		}

		requiredVDev, err := VDevicesByIDs(m.vDevices, req.MustIncludeDeviceIDs)
		if err != nil {
			return nil, fmt.Errorf("Unable to retrieve list of available vdevices: %v", err)
		}
		required, err := gpuallocator.NewDevicesFrom(UniqueDeviceIDs(requiredVDev))
		if err != nil {
			return nil, fmt.Errorf("Unable to retrieve list of required devices: %v", err)
		}

		allocated := m.allocatePolicy.Allocate(available, required, int(req.AllocationSize))

		var deviceIds []string
		for _, device := range allocated {
			for _, vd := range availableVDev {
				if vd.dev.ID == device.UUID {
					deviceIds = append(deviceIds, vd.ID)
					break
				}
			}
		}

		resp := &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: deviceIds,
		}

		response.ContainerResponses = append(response.ContainerResponses, resp)
	}
	//return nil, fmt.Errorf("Not implemented")
	return response, nil
}

func checkstatus(m *NvidiaDevicePlugin) {
	fmt.Println("Allocate:Checking status:vdevices=", m.vDevices, "remaining=", m.remaining, "occupancy=", m.vidoccupy)
}

// Allocate which return list of devices.
func (m *NvidiaDevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	responses := pluginapi.AllocateResponse{}
	checkstatus(m)
	for _, req := range reqs.ContainerRequests {
		vdevices, err := VDevicesByIDs(m.vDevices, req.DevicesIDs)
		if err != nil {
			return nil, err
		}
		freeOccupy(m, vdevices)
	}

	for _, req := range reqs.ContainerRequests {
		fmt.Println("req__=", req.DevicesIDs)
		vdevices, err := VDevicesByIDs(m.vDevices, req.DevicesIDs)
		if err != nil {
			return nil, err
		}

		response := pluginapi.ContainerAllocateResponse{}

		uuids := allocateAndGet(m, len(req.DevicesIDs))
		addOccupancy(m, vdevices)

		//uuids := UniqueDeviceIDs(vdevices)
		deviceIDs := m.deviceIDsFromUUIDs(uuids)

		if deviceListStrategyFlag == DeviceListStrategyEnvvar {
			response.Envs = m.apiEnvs(m.deviceListEnvvar, deviceIDs)
		}
		if deviceListStrategyFlag == DeviceListStrategyVolumeMounts {
			response.Envs = m.apiEnvs(m.deviceListEnvvar, []string{deviceListAsVolumeMountsContainerPathRoot})
			response.Mounts = m.apiMounts(deviceIDs)
		}
		if passDeviceSpecsFlag {
			response.Devices = m.apiDeviceSpecs(nvidiaDriverRootFlag, uuids)
		}

		var mapEnvs []string
		for i, vd := range vdevices {
			limitKey := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%v", i)
			response.Envs[limitKey] = fmt.Sprintf("%vm", vd.memory)
			mapEnvs = append(mapEnvs, fmt.Sprintf("%v:%v", i, vd.dev.ID))
		}
		response.Envs["CUDA_DEVICE_SM_LIMIT"] = strconv.Itoa(int(100 * deviceCoresScalingFlag / float64(deviceSplitCountFlag)))
		response.Envs["NVIDIA_DEVICE_MAP"] = strings.Join(mapEnvs, " ")
		response.Envs["CUDA_DEVICE_MEMORY_SHARED_CACHE"] = fmt.Sprintf("/tmp/%v.cache", uuid.NewString())
		if deviceMemoryScalingFlag > 1 {
			response.Envs["CUDA_OVERSUBSCRIBE"] = "true"
		}
		response.Mounts = append(response.Mounts,
			&pluginapi.Mount{ContainerPath: "/usr/local/vgpu/libvgpu.so",
				HostPath: "/usr/local/vgpu/libvgpu.so", ReadOnly: true},
			&pluginapi.Mount{ContainerPath: "/etc/ld.so.preload",
				HostPath: "/usr/local/vgpu/ld.so.preload", ReadOnly: true})
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	return &responses, nil
}

// PreStartContainer is unimplemented for this plugin
func (m *NvidiaDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func (m *NvidiaDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}

func (m *NvidiaDevicePlugin) deviceExists(id string) bool {
	for _, d := range m.cachedDevices {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (m *NvidiaDevicePlugin) deviceIDsFromUUIDs(uuids []string) []string {
	if deviceIDStrategyFlag == DeviceIDStrategyUUID {
		return uuids
	}

	var deviceIDs []string
	if deviceIDStrategyFlag == DeviceIDStrategyIndex {
		for _, d := range m.cachedDevices {
			for _, id := range uuids {
				if d.ID == id {
					deviceIDs = append(deviceIDs, d.Index)
				}
			}
		}
	}
	return deviceIDs
}

func (m *NvidiaDevicePlugin) apiDevices() []*pluginapi.Device {
	var pdevs []*pluginapi.Device
	for _, d := range m.vDevices {
		d.Health = d.dev.Health
		pdevs = append(pdevs, &d.Device)
	}
	return pdevs
}

func (m *NvidiaDevicePlugin) apiEnvs(envvar string, deviceIDs []string) map[string]string {
	return map[string]string{
		envvar: strings.Join(deviceIDs, ","),
	}
}

func (m *NvidiaDevicePlugin) apiMounts(deviceIDs []string) []*pluginapi.Mount {
	var mounts []*pluginapi.Mount

	for _, id := range deviceIDs {
		mount := &pluginapi.Mount{
			HostPath:      deviceListAsVolumeMountsHostPath,
			ContainerPath: filepath.Join(deviceListAsVolumeMountsContainerPathRoot, id),
		}
		mounts = append(mounts, mount)
	}

	return mounts
}

func (m *NvidiaDevicePlugin) apiDeviceSpecs(driverRoot string, uuids []string) []*pluginapi.DeviceSpec {
	var specs []*pluginapi.DeviceSpec

	paths := []string{
		"/dev/nvidiactl",
		"/dev/nvidia-uvm",
		"/dev/nvidia-uvm-tools",
		"/dev/nvidia-modeset",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			spec := &pluginapi.DeviceSpec{
				ContainerPath: p,
				HostPath:      filepath.Join(driverRoot, p),
				Permissions:   "rw",
			}
			specs = append(specs, spec)
		}
	}

	for _, d := range m.cachedDevices {
		for _, id := range uuids {
			if d.ID == id {
				for _, p := range d.Paths {
					spec := &pluginapi.DeviceSpec{
						ContainerPath: p,
						HostPath:      filepath.Join(driverRoot, p),
						Permissions:   "rw",
					}
					specs = append(specs, spec)
				}
			}
		}
	}

	return specs
}
