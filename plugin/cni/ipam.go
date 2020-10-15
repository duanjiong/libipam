// Copyright (c) 2015-2020 Tigera, Inc. All rights reserved.
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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"kubesphere.io/libipam/lib/utils"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	cniSpecVersion "github.com/containernetworking/cni/pkg/version"
	"github.com/projectcalico/libcalico-go/lib/apiconfig"
	"github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/logutils"
	"github.com/projectcalico/libcalico-go/lib/names"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/options"
	"github.com/sirupsen/logrus"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
	"kubesphere.io/libipam/lib/apis/v1alpha1"
	"kubesphere.io/libipam/lib/client"
	"kubesphere.io/libipam/lib/ipam"
)

// Kubernetes a K8s specific struct to hold config
type Kubernetes struct {
	Kubeconfig string `json:"kubeconfig"`
}

// NetConf stores the common network config for Calico CNI plugin
type NetConf struct {
	CNIVersion string `json:"cniVersion,omitempty"`
	Name       string `json:"name"`
	Type       string `json:"type"`

	LogLevel        string `json:"log_level"`
	LogFilePath     string `json:"log_file_path"`
	LogFileMaxSize  int    `json:"log_file_max_size"`
	LogFileMaxAge   int    `json:"log_file_max_age"`
	LogFileMaxCount int    `json:"log_file_max_count"`

	DatastoreType string     `json:"datastore_type"`
	Kubernetes    Kubernetes `json:"kubernetes"`
}

type ipamArgs struct {
	cnitypes.CommonArgs
	Type                       cnitypes.UnmarshallableString
	K8S_POD_NAME               cnitypes.UnmarshallableString
	K8S_POD_NAMESPACE          cnitypes.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID cnitypes.UnmarshallableString
	K8S_POD_INFRA_VM_ID        cnitypes.UnmarshallableString
	Pool                       cnitypes.UnmarshallableString
}

func main() {
	// Set up logging formatting.
	logrus.SetFormatter(&logutils.Formatter{})

	// Install a hook that adds file/line no information.
	logrus.AddHook(&logutils.ContextHook{})

	// Display the version on "-v", otherwise just delegate to the skel code.
	// Use a new flag set so as not to conflict with existing libraries which use "flag"
	flagSet := flag.NewFlagSet("ks-ipam", flag.ExitOnError)

	versionFlag := flagSet.Bool("v", false, "Display version")
	err := flagSet.Parse(os.Args[1:])

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if *versionFlag {
		os.Exit(0)
	}

	skel.PluginMain(cmdAdd, nil, cmdDel,
		cniSpecVersion.PluginSupports("0.1.0", "0.2.0", "0.3.0", "0.3.1"), "Kubesphere IPAM")
}

func getHandleID(args ipamArgs) string {
	handleID := ""
	switch args.Type {
	case MACVTAP:
		handleID = fmt.Sprintf("%s-%s-%s", args.Type, args.K8S_POD_NAMESPACE, args.K8S_POD_INFRA_VM_ID)
	default:
		handleID = fmt.Sprintf("%s-%s-%s", args.Type, args.K8S_POD_NAMESPACE, args.K8S_POD_INFRA_CONTAINER_ID)
	}
	return handleID
}

func ConfigureLogging(conf NetConf) {
	if strings.EqualFold(conf.LogLevel, "debug") {
		logrus.SetLevel(logrus.DebugLevel)
	} else if strings.EqualFold(conf.LogLevel, "info") {
		logrus.SetLevel(logrus.InfoLevel)
	} else if strings.EqualFold(conf.LogLevel, "error") {
		logrus.SetLevel(logrus.ErrorLevel)
	} else {
		// Default level
		logrus.SetLevel(logrus.WarnLevel)
	}

	writers := []io.Writer{os.Stderr}
	// Set the log output to write to a log file if specified.
	if conf.LogFilePath != "" {
		// Create the path for the log file if it does not exist
		err := os.MkdirAll(filepath.Dir(conf.LogFilePath), 0755)
		if err != nil {
			logrus.WithError(err).Errorf("Failed to create path for CNI log file: %v", filepath.Dir(conf.LogFilePath))
		}

		// Create file logger with log file rotation.
		fileLogger := &lumberjack.Logger{
			Filename:   conf.LogFilePath,
			MaxSize:    100,
			MaxAge:     30,
			MaxBackups: 10,
		}

		// Set the max size if exists. Defaults to 100 MB.
		if conf.LogFileMaxSize != 0 {
			fileLogger.MaxSize = conf.LogFileMaxSize
		}

		// Set the max time in days to retain a log file before it is cleaned up. Defaults to 30 days.
		if conf.LogFileMaxAge != 0 {
			fileLogger.MaxAge = conf.LogFileMaxAge
		}

		// Set the max number of log files to retain before they are cleaned up. Defaults to 10.
		if conf.LogFileMaxCount != 0 {
			fileLogger.MaxBackups = conf.LogFileMaxCount
		}

		writers = append(writers, fileLogger)
	}

	mw := io.MultiWriter(writers...)

	logrus.SetOutput(mw)
}

func CreateClient(conf NetConf) (client.Interface, error) {
	if conf.DatastoreType != "" {
		if err := os.Setenv("DATASTORE_TYPE", conf.DatastoreType); err != nil {
			return nil, err
		}
	}

	// Set Kubernetes specific variables for use with the Kubernetes libcalico backend.
	if conf.Kubernetes.Kubeconfig != "" {
		if err := os.Setenv("KUBECONFIG", conf.Kubernetes.Kubeconfig); err != nil {
			return nil, err
		}
	}

	// Load the client config from the current environment.
	clientConfig, err := apiconfig.LoadClientConfig("")
	if err != nil {
		return nil, err
	}

	// Create a new client.
	calicoClient, err := client.New(*clientConfig)
	if err != nil {
		return nil, err
	}
	return calicoClient, nil
}

const (
	MACVTAP = "macvtap"
)

func parseIpamArgs(args string) (ipamArgs, error) {
	ipamArgs := ipamArgs{}
	if err := cnitypes.LoadArgs(args, &ipamArgs); err != nil {
		return ipamArgs, err
	}

	switch ipamArgs.Type {
	case MACVTAP:
		if ipamArgs.K8S_POD_NAMESPACE == "" || ipamArgs.K8S_POD_INFRA_VM_ID == "" || ipamArgs.Pool == "" {
			return ipamArgs, fmt.Errorf("K8S_POD_NAMESPACE/K8S_POD_INFRA_VM_ID/Pool should not be empty")
		}
	default:
		if ipamArgs.K8S_POD_NAMESPACE == "" || ipamArgs.K8S_POD_NAME == "" || ipamArgs.K8S_POD_INFRA_CONTAINER_ID == "" {
			return ipamArgs, fmt.Errorf("K8S_POD_NAMESPACE/K8S_POD_NAME/K8S_POD_INFRA_CONTAINER_ID should not be empty")
		}
	}

	return ipamArgs, nil
}

func fillAutoAssignArgs(pools []v1alpha1.IPPool, args ipamArgs) ipam.AutoAssignArgs {
	handleID := getHandleID(args)
	assignArgs := ipam.AutoAssignArgs{
		HandleID: &handleID,
	}

	switch args.Type {
	case MACVTAP:
		assignArgs.Num4 = 1
		assignArgs.Hostname = MACVTAP

		attrs := map[string]string{}
		attrs[ipam.AttributeVm] = string(args.K8S_POD_INFRA_VM_ID)
		attrs[ipam.AttributeNamespace] = string(args.K8S_POD_NAMESPACE)
		attrs[ipam.AttributeType] = string(args.Type)
	default:
		//TODO
		assignArgs.Num4 = 1
		assignArgs.Hostname, _ = names.Hostname()

		attrs := map[string]string{}
		attrs[ipam.AttributePod] = string(args.K8S_POD_NAME)
		attrs[ipam.AttributeNamespace] = string(args.K8S_POD_NAMESPACE)
		attrs[ipam.AttributeType] = string(args.Type)
	}

	for _, ipp := range pools {
		// Found a match. Use the CIDR from the matching pool.
		_, cidr, _ := net.ParseCIDR(ipp.Spec.CIDR)
		logrus.Infof("Resolved pool name %s to cidr %s", ipp.Name, cidr)
		assignArgs.IPv4Pools = append(assignArgs.IPv4Pools, cnet.IPNet{IPNet: *cidr})
		assignArgs.ID = ipp.ID()

		switch args.Type {
		case MACVTAP:
			ones, bits := cidr.Mask.Size()
			if ipp.Spec.RangeStart != "" && ipp.Spec.RangeEnd != "" {
				assignArgs.HostReservedAttrIPv4s = &ipam.HostReservedAttr{
					StartOfBlock: int(utils.IP2Int(net.ParseIP(ipp.Spec.RangeStart)) - utils.IP2Int(cidr.IP)),
					EndOfBlock:   int(utils.IP2Int(cidr.IP) + uint32(math.Pow(2, float64(bits-ones))) - utils.IP2Int(net.ParseIP(ipp.Spec.RangeEnd)) - 1),
					Handle:       MACVTAP,
					Note:         "macvtap reserve",
				}
			}
		}
	}

	return assignArgs
}

func cmdAdd(args *skel.CmdArgs) error {
	conf := NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	ConfigureLogging(conf)

	client, err := CreateClient(conf)
	if err != nil {
		return err
	}

	ipamArgs, err := parseIpamArgs(args.Args)
	if err != nil {
		return err
	}

	handleID := getHandleID(ipamArgs)
	logger := logrus.WithFields(logrus.Fields{
		"HandleID": handleID,
	})

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	r := &current.Result{}

	pl, err := client.IPPools().List(ctx, options.ListOptions{})
	if err != nil {
		return err
	}

	var (
		assignArgs   ipam.AutoAssignArgs
		requestPools []v1alpha1.IPPool
		resultPool   v1alpha1.IPPool
	)
	for _, ipp := range pl.Items {
		if ipp.Name == string(ipamArgs.Pool) {
			requestPools = append(requestPools, ipp)
			break
		}
	}

	assignArgs = fillAutoAssignArgs(requestPools, ipamArgs)

	logger.WithField("assignArgs", assignArgs).Info("Auto assigning IP")
	assignedV4, assignedV6, err := client.IPAM().AutoAssign(ctx, assignArgs)
	logger.Infof("Kubesphere CNI IPAM assigned addresses IPv4=%v IPv6=%v", assignedV4, assignedV6)
	if err != nil {
		return err
	}

	if len(assignedV4) != assignArgs.Num4 {
		return fmt.Errorf("failed to request %d IPv4 addresses. IPAM allocated only %d", assignArgs.Num4, len(assignedV4))
	}

	ipV4Network := net.IPNet{IP: assignedV4[0].IP, Mask: assignedV4[0].Mask}
	r.IPs = append(r.IPs, &current.IPConfig{
		Version: "4",
		Address: ipV4Network,
	})

	for _, ipp := range pl.Items {
		if ipp.ID() != assignArgs.ID {
			continue
		}
		_, tmp, _ := cnet.ParseCIDR(ipp.Spec.CIDR)
		if tmp.Contains(assignedV4[0].IP) {
			resultPool = ipp
			break
		}
	}
	for _, route := range resultPool.Spec.Routes {
		_, dst, _ := net.ParseCIDR(route.Dst)
		r.Routes = append(r.Routes, &cnitypes.Route{
			Dst: *dst,
			GW:  net.ParseIP(route.GW),
		})
	}
	r.DNS.Domain = resultPool.Spec.DNS.Domain
	r.DNS.Options = resultPool.Spec.DNS.Options
	r.DNS.Nameservers = resultPool.Spec.DNS.Nameservers
	r.DNS.Search = resultPool.Spec.DNS.Search

	if ipamArgs.Type == MACVTAP {
		r.Interfaces = append(r.Interfaces, &current.Interface{
			Name:    args.IfName,
			Mac:     ethRandomAddr(assignedV4[0].IP),
			Sandbox: "",
		})
	}

	logger.WithFields(logrus.Fields{"result": r}).Debug("IPAM Result")

	// Print result to stdout, in the format defined by the requested cniVersion.
	return cnitypes.PrintResult(r, "0.3.0")
}

func ethRandomAddr(ip net.IP) string {
	buf := make([]byte, 2)
	rand.Read(buf)

	// Clear multicast bit
	buf[0] &= 0xfe
	// Set the local bit
	buf[0] |= 2

	//TODO support ipv6
	buf = append(buf, []byte(ip)...)
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
}

func cmdDel(args *skel.CmdArgs) error {
	conf := NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	ConfigureLogging(conf)

	client, err := CreateClient(conf)
	if err != nil {
		return err
	}

	ipamArgs, err := parseIpamArgs(args.Args)
	if err != nil {
		return err
	}

	handleID := getHandleID(ipamArgs)
	logger := logrus.WithFields(logrus.Fields{
		"HandleID": handleID,
	})

	logger.Info("Releasing address using handleID")
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	if err := client.IPAM().ReleaseByHandle(ctx, handleID); err != nil {
		if _, ok := err.(errors.ErrorResourceDoesNotExist); !ok {
			logger.WithError(err).Error("Failed to release address")
			return err
		}
		logger.Warn("Asked to release address but it doesn't exist. Ignoring")
	} else {
		logger.Info("Released address using handleID")
	}

	return nil
}
