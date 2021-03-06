// Copyright 2017 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"

	"github.com/intel/intel-device-plugins-for-kubernetes/internal/deviceplugin"
)

const (
	sysfsDirectory = "/sys/class/fpga"
	devfsDirectory = "/dev"
	deviceRE       = `^intel-fpga-dev.[0-9]+$`
	portRE         = `^intel-fpga-port.[0-9]+$`
	fmeRE          = `^intel-fpga-fme.[0-9]+$`

	// Device plugin settings.
	pluginEndpointPrefix = "intel-fpga"
	resourceNamePrefix   = "intel.com/fpga"
)

type pluginMode string

const (
	afMode     pluginMode = "af"
	regionMode pluginMode = "region"
)

func isValidPluginMode(m pluginMode) bool {
	return m == afMode || m == regionMode
}

// deviceManager manages Intel FPGA devices.
type deviceManager struct {
	srv     deviceplugin.Server
	fpgaID  string
	name    string
	devices map[string]deviceplugin.DeviceInfo
	root    string
	mode    pluginMode
}

func newDeviceManager(resourceName string, fpgaID string, rootDir string, mode pluginMode) *deviceManager {
	return &deviceManager{
		fpgaID:  fpgaID,
		name:    resourceName,
		devices: make(map[string]deviceplugin.DeviceInfo),
		root:    rootDir,
		mode:    mode,
	}
}

// Discovers all FPGA devices available on the local node by walking `/sys/class/fpga` directory.
func discoverFPGAs(sysfsDir string, devfsDir string, mode pluginMode) (map[string]map[string]deviceplugin.DeviceInfo, error) {
	deviceReg := regexp.MustCompile(deviceRE)
	portReg := regexp.MustCompile(portRE)
	fmeReg := regexp.MustCompile(fmeRE)

	result := make(map[string]map[string]deviceplugin.DeviceInfo)

	fpgaFiles, err := ioutil.ReadDir(sysfsDir)
	if err != nil {
		return nil, fmt.Errorf("Can't read sysfs folder: %v", err)
	}

	for _, fpgaFile := range fpgaFiles {
		fname := fpgaFile.Name()
		if deviceReg.MatchString(fname) {
			var interfaceID string

			deviceFolder := path.Join(sysfsDir, fname)
			deviceFiles, err := ioutil.ReadDir(deviceFolder)
			if err != nil {
				return nil, err
			}
			fpgaNodes := make(map[string][]string)

			if mode == regionMode {
				for _, deviceFile := range deviceFiles {
					name := deviceFile.Name()
					if fmeReg.MatchString(name) {
						if len(interfaceID) > 0 {
							return nil, fmt.Errorf("Detected more than one FPGA region for device %s. Only one region per FPGA device is supported", fname)
						}
						interfaceIDFile := path.Join(deviceFolder, name, "pr", "interface_id")
						data, err := ioutil.ReadFile(interfaceIDFile)
						if err != nil {
							return nil, err
						}
						interfaceID = fmt.Sprintf("%s-%s", mode, strings.TrimSpace(string(data)))
						fpgaNodes[interfaceID] = append(fpgaNodes[interfaceID], name)
					}
				}
			}

			for _, deviceFile := range deviceFiles {
				name := deviceFile.Name()
				if portReg.MatchString(name) {
					switch mode {
					case regionMode:
						if len(interfaceID) == 0 {
							return nil, fmt.Errorf("No FPGA region found for %s", fname)
						}
						fpgaNodes[interfaceID] = append(fpgaNodes[interfaceID], name)
					case afMode:
						afuFile := path.Join(deviceFolder, name, "afu_id")
						data, err := ioutil.ReadFile(afuFile)
						if err != nil {
							return nil, err
						}
						afuID := fmt.Sprintf("%s-%s", mode, strings.TrimSpace(string(data)))
						fpgaNodes[afuID] = append(fpgaNodes[afuID], name)
					default:
						glog.Fatal("Unsupported mode")
					}
				}
			}
			if len(fpgaNodes) == 0 {
				return nil, fmt.Errorf("No device nodes found for %s", fname)
			}
			for fpgaID, nodes := range fpgaNodes {
				var devNodes []string
				for _, node := range nodes {
					devNode := path.Join(devfsDir, node)
					if _, err := os.Stat(devNode); err != nil {
						return nil, fmt.Errorf("Device %s doesn't exist: %+v", devNode, err)
					}
					devNodes = append(devNodes, devNode)
				}
				sort.Strings(devNodes)
				if _, ok := result[fpgaID]; !ok {
					result[fpgaID] = make(map[string]deviceplugin.DeviceInfo)
				}
				result[fpgaID][fname] = deviceplugin.DeviceInfo{
					State: pluginapi.Healthy,
					Nodes: devNodes,
				}
			}
		}
	}
	return result, nil
}

func (dm *deviceManager) GetDevicePluginOptions(ctx context.Context, empty *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	fmt.Println("GetDevicePluginOptions: return empty options")
	return new(pluginapi.DevicePluginOptions), nil
}

// ListAndWatch returns a list of devices
// Whenever a device state change or a device disappears, ListAndWatch returns the new list
func (dm *deviceManager) ListAndWatch(empty *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	sysfsDir := path.Join(dm.root, sysfsDirectory)
	devfsDir := path.Join(dm.root, devfsDirectory)
	for {
		devs, err := discoverFPGAs(sysfsDir, devfsDir, dm.mode)
		if err != nil {
			dm.srv.Stop()
			return fmt.Errorf("Device discovery failed: %+v", err)
		}
		devinfos, ok := devs[dm.fpgaID]
		if !ok {
			dm.srv.Stop()
			return fmt.Errorf("AFU id %s disappeared", dm.fpgaID)
		}
		if !reflect.DeepEqual(dm.devices, devinfos) {
			dm.devices = devinfos
			resp := new(pluginapi.ListAndWatchResponse)
			var keys []string
			for key := range dm.devices {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, id := range keys {
				resp.Devices = append(resp.Devices, &pluginapi.Device{id, dm.devices[id].State})
			}
			if err := stream.Send(resp); err != nil {
				dm.srv.Stop()
				return fmt.Errorf("device-plugin: cannot update device list: %v", err)
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func (dm *deviceManager) Allocate(ctx context.Context, rqt *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	return deviceplugin.MakeAllocateResponse(rqt, dm.devices)
}

func (dm *deviceManager) PreStartContainer(ctx context.Context, rqt *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	glog.Warning("PreStartContainer() should not be called")
	return new(pluginapi.PreStartContainerResponse), nil
}

func main() {
	var modeStr string

	flag.StringVar(&modeStr, "mode", string(afMode), fmt.Sprintf("device plugin mode: '%s' (default) or '%s'", afMode, regionMode))
	flag.Parse()

	mode := pluginMode(modeStr)
	if !isValidPluginMode(mode) {
		glog.Error("Wrong mode: ", modeStr)
		os.Exit(1)
	}

	fmt.Println("FPGA device plugin started in", mode, "mode")

	devs, err := discoverFPGAs(sysfsDirectory, devfsDirectory, mode)
	if err != nil {
		glog.Fatalf("Device discovery failed: %+v", err)
	}

	if len(devs) == 0 {
		glog.Error("No devices found. Waiting indefinitely.")
		select {}
	}

	ch := make(chan error)
	for fpgaID := range devs {
		resourceName := resourceNamePrefix + "-" + fpgaID
		pPrefix := pluginEndpointPrefix + "-" + fpgaID
		dm := newDeviceManager(resourceName, fpgaID, "/", mode)

		go func() {
			ch <- dm.srv.Serve(dm, resourceName, pPrefix)
		}()
	}
	err = <-ch
	if err != nil {
		glog.Fatal(err)
	}
}
