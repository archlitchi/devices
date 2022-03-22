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
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"k8s.io/klog"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"volcano.sh/k8s-device-plugin/pkg/filewatcher"
	"volcano.sh/k8s-device-plugin/pkg/plugin"
	"volcano.sh/k8s-device-plugin/pkg/plugin/config"
	"volcano.sh/k8s-device-plugin/pkg/plugin/nvidia"
	"volcano.sh/k8s-device-plugin/pkg/plugin/util"
	"volcano.sh/k8s-device-plugin/pkg/plugin/version"
	"volcano.sh/k8s-device-plugin/pkg/protos"
	"volcano.sh/k8s-device-plugin/pkg/server"
)

var (
	failOnInitErrorFlag bool
	//nvidiaDriverRootFlag string
	//enableLegacyPreferredFlag bool
	migStrategyFlag string

	rootCmd = &cobra.Command{
		Use:   "k8s-device-plugin",
		Short: "kubernetes vgpu device-plugin",
		Run: func(cmd *cobra.Command, args []string) {
			if err := start(); err != nil {
				klog.Fatal(err)
			}
		},
	}
)

func init() {
	// https://github.com/spf13/viper/issues/461
	viper.BindEnv("node-name", "NODE_NAME")

	rootCmd.Flags().SortFlags = false
	rootCmd.PersistentFlags().SortFlags = false

	rootCmd.Flags().StringVar(&migStrategyFlag, "mig-strategy", "none", "the desired strategy for exposing MIG devices on GPUs that support it:\n\t\t[none | single | mixed]")
	rootCmd.Flags().BoolVar(&failOnInitErrorFlag, "fail-on-init-error", true, "fail the plugin if an error is encountered during initialization, otherwise block indefinitely")
	rootCmd.Flags().StringVar(&config.RuntimeSocketFlag, "runtime-socket", "/var/lib/vgpu/vgpu.sock", "runtime socket")
	rootCmd.Flags().UintVar(&config.DeviceSplitCount, "device-split-count", 2, "the number for NVIDIA device split")
	rootCmd.Flags().Float64Var(&config.DeviceMemoryScaling, "device-memory-scaling", 1.0, "the ratio for NVIDIA device memory scaling")
	rootCmd.Flags().Float64Var(&config.DeviceCoresScaling, "device-cores-scaling", 1.0, "the ratio for NVIDIA device cores scaling")
	rootCmd.Flags().StringVar(&config.SchedulerEndpoint, "scheduler-endpoint", "127.0.0.1:9090", "scheduler extender endpoint")
	rootCmd.Flags().IntVar(&config.SchedulerTimeout, "scheduler-timeout", 10, "scheduler connection timeout")
	rootCmd.Flags().StringVar(&config.NodeName, "node-name", viper.GetString("node-name"), "node name")

	rootCmd.PersistentFlags().AddGoFlagSet(util.GlobalFlagSet())
	rootCmd.AddCommand(version.VersionCmd)
}

func getAllPlugins() []plugin.DevicePlugin {
	return []plugin.DevicePlugin{
		nvidia.NewNvidiaDevicePlugin(),
	}
}

func start() error {
	//flag.Parse()

	log.Println("Starting file watcher.")
	watcher, err := filewatcher.NewFileWatcher(pluginapi.DevicePluginPath)
	if err != nil {
		log.Printf("Failed to created file watcher: %v", err)
		os.Exit(1)
	}

	log.Println("Retrieving plugins.")
	plugins := getAllPlugins()

	log.Println("Starting OS signal watcher.")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	cache := nvidia.NewDeviceCache()
	cache.Start()
	defer cache.Stop()

	register := nvidia.NewDeviceRegister(cache)
	register.Start()
	defer register.Stop()

	// start runtime grpc server
	lisGrpc, err := net.Listen("unix", protos.RuntimeSocketFlag)
	if err != nil {
		klog.Fatalf("bind unix socket %v failed, %v", err)
	}
	defer lisGrpc.Close()
	runtimeServer := grpc.NewServer()

	rt := server.NewVGPURuntimeService(cache)
	protos.RegisterVGPURuntimeServiceServer(runtimeServer, rt)
	go func() {
		err := runtimeServer.Serve(lisGrpc)
		if err != nil {
			klog.Fatal(err)
		}
	}()
	defer runtimeServer.Stop()

	go func() {
		select {
		case s := <-sigCh:
			log.Printf("Received signal \"%v\", shutting down.", s)
			for _, p := range plugins {
				p.Stop()
			}
		}
		os.Exit(-1)
	}()

restart:
	// Loop through all plugins, idempotently stopping them, and then starting
	// them if they have any devices to serve. If even one plugin fails to
	// start properly, try starting them all again.
	for _, p := range plugins {
		p.Stop()

		// Just continue if there are no devices to serve for plugin p.
		if p.DevicesNum() == 0 {
			continue
		}

		// Start the gRPC server for plugin p and connect it with the kubelet.
		if err := p.Start(); err != nil {
			log.Printf("Plugin %s failed to start: %v", p.Name(), err)
			log.Printf("You can check the prerequisites at: https://github.com/volcano-sh/k8s-device-plugin#prerequisites")
			log.Printf("You can learn how to set the runtime at: https://github.com/volcano-sh/k8s-device-plugin#quick-start")
			// If there was an error starting any plugins, restart them all.
			goto restart
		}
	}

	// Start an infinite loop, waiting for several indicators to either log
	// some messages, trigger a restart of the plugins, or exit the program.
	for {
		select {
		// Detect a kubelet restart by watching for a newly created
		// 'pluginapi.KubeletSocket' file. When this occurs, restart this loop,
		// restarting all of the plugins in the process.
		case event := <-watcher.Events:
			if event.Name == pluginapi.KubeletSocket && event.Op&fsnotify.Create == fsnotify.Create {
				log.Printf("inotify: %s created, restarting.", pluginapi.KubeletSocket)
				goto restart
			}

		// Watch for any other fs errors and log them.
		case err := <-watcher.Errors:
			log.Printf("inotify: %s", err)
		}
	}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		klog.Fatal(err)
	}
}
