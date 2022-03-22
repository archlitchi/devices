/*
 * Copyright © 2021 peizhaoyou <peizhaoyou@4paradigm.com>
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

package nvidia

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"k8s.io/klog"
	"volcano.sh/k8s-device-plugin/pkg/protos"

	"google.golang.org/grpc"
)

//type DevListFunc func() []*Device

type DeviceRegister struct {
	deviceCache *DeviceCache
	unhealthy   chan *Device
	stopCh      chan struct{}
}

func NewDeviceRegister(deviceCache *DeviceCache) *DeviceRegister {
	return &DeviceRegister{
		deviceCache: deviceCache,
		unhealthy:   make(chan *Device),
		stopCh:      make(chan struct{}),
	}
}

func (r *DeviceRegister) Start() {
	r.deviceCache.AddNotifyChannel("register", r.unhealthy)
	go r.WatchAndRegister()
}

func (r *DeviceRegister) Stop() {
	close(r.stopCh)
}

func (r *DeviceRegister) apiDevices() *[]*protos.DeviceInfo {
	devs := r.deviceCache.GetCache()
	res := make([]*protos.DeviceInfo, 0, len(devs))
	for _, dev := range devs {
		ndev, err := nvml.NewDeviceByUUID(dev.ID)
		if err != nil {
			fmt.Println("nvml new device by uuid error id=", dev.ID)
			panic(0)
		} else {
			fmt.Println("nvml registered device id=", dev.ID, "memory=", *ndev.Memory)
		}
		registeredmem := int32(*ndev.Memory)
		/*if config.DeviceMemoryScaling > 1 {
			fmt.Println("Memory Scaling to", config.DeviceMemoryScaling)
			registeredmem = registeredmem * int32(config.DeviceMemoryScaling)
		}*/
		res = append(res, &protos.DeviceInfo{
			Id: dev.ID,
			//Count:  int32(config.DeviceSplitCount),
			Count:  10,
			Devmem: registeredmem,
			Health: dev.Health == "healthy",
		})
	}
	return &res
}

func (r *DeviceRegister) Register(ctx context.Context) error {
	log.Printf("into Register")
	conn, err := grpc.DialContext(
		ctx,
		VolcanoSchedulerEndpoint,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		//grpc.WithConnectParams(grpc.ConnectParams{MinConnectTimeout: 3}),
	)
	if err != nil {
		klog.Errorf("connect scheduler error %v", err)
		return err
	}
	log.Printf("into device register")
	client := protos.NewDeviceServiceClient(conn)
	register, err := client.Register(ctx)
	if err != nil {
		klog.Errorf("register error %v", err)
		err = fmt.Errorf("client register error, %v", err)
		return err
	}
	req := protos.RegisterRequest{Node: os.Getenv("NODE_NAME"), Devices: *r.apiDevices()}
	err = register.Send(&req)
	if err != nil {
		log.Printf("register send error, %v", err)
		return err
	}
	//klog.V(3).Infof("register info %v", req.String())
	log.Printf("register info %v", req.String())
	closeCh := make(chan struct{})
	go func() {
		reply := protos.RegisterReply{}
		err := register.RecvMsg(reply)
		if err != nil {
			//klog.Errorf("register recv error, %v", err)
			log.Printf("register recv error, %v", err)
		} else {
			//klog.Errorf("register recv closed")
			log.Printf("register recv closed")
		}
		closeCh <- struct{}{}
	}()
	for {
		select {
		case <-r.unhealthy:
			err = register.Send(&protos.RegisterRequest{
				Node:    os.Getenv("NODE_NAME"),
				Devices: *r.apiDevices(),
			})
			if err != nil {
				klog.Errorf("register send error, %v", err)
				return err
			}
			klog.V(3).Infof("register info %v", req.String())
		case <-closeCh:
			klog.Infof("register server closed")
			return fmt.Errorf("register server closed")
		case <-r.stopCh:
			return nil
		}
	}
}

func (r *DeviceRegister) WatchAndRegister() {
	//ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	//defer cancel()
	ctx := context.Background()
	for {
		err := r.Register(ctx)
		if err != nil {
			klog.Errorf("register error, %v", err)
			time.Sleep(time.Second * 5)
		} else {
			klog.Infof("register stopped")
			break
		}
	}
}
