// Copyright 2018 Authors of Cilium
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

package sockops

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/datapath/loader"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/mountinfo"
	"github.com/cilium/cilium/pkg/option"

	"github.com/sirupsen/logrus"
)

var (
	// Path to where cgroup is mounted
	cgroupRoot = defaults.DefaultCgroupRoot

	// Only mount a single instance
	cgrpMountOnce sync.Once

	// Default prefix for map objects
	mapPrefix = defaults.DefaultMapPrefix

	contextTimeout = 5 * time.Minute
)

const (
	msgVerdict = "msg_verdict"
	skbVerdict = "stream_verdict"
	skbParser  = "stream_parser"

	cSockops = "bpf_sockops.c"
	oSockops = "bpf_sockops.o"
	eSockops = "bpf_sockops"

	cIPC = "bpf_redir.c"
	oIPC = "bpf_redir.o"
	eIPC = "bpf_redir"

	cskbIPC = "bpf_redir_ing.c"
	oskbIPC = "bpf_redir_ing.o"
	eskbIPC = "bpf_redir_ing"

	cparserIPC = "bpf_redir_parser.c"
	oparserIPC = "bpf_redir_parser.o"
	eparserIPC = "bpf_redir_parser"

	cKtlsUp = "bpf_ktls_up.c"
	oKtlsUp = "bpf_ktls_up.o"
	eKtlsUp = "bpf_ktls_up"

	cKtlsDown = "bpf_ktls_down.c"
	oKtlsDown = "bpf_ktls_down.o"
	eKtlsDown = "bpf_ktls_down"

	sockMap         = "sock_ops_map"
	sockKtlsUpMap   = "sock_ops_ktls_up"
	sockKtlsDownMap = "sock_ops_ktls_down"
)

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, "sockops")

// setCgroupRoot will set the path to mount cgroupv2
func setCgroupRoot(path string) {
	cgroupRoot = path
}

// mountCgroup mounts the Cgroup v2 filesystem into the desired cgroupRoot directory.
func mountCgroup() error {
	cgroupRootStat, err := os.Stat(cgroupRoot)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(cgroupRoot, 0755); err != nil {
				return fmt.Errorf("Unable to create cgroup mount directory: %s", err)
			}
		} else {
			return fmt.Errorf("Failed to stat the mount path %s: %s", cgroupRoot, err)
		}
	} else if !cgroupRootStat.IsDir() {
		return fmt.Errorf("%s is a file which is not a directory", cgroupRoot)
	}

	if err := syscall.Mount("none", cgroupRoot, mountinfo.FilesystemTypeCgroup2, 0, ""); err != nil {
		return fmt.Errorf("failed to mount %s: %s", cgroupRoot, err)
	}

	return nil
}

// checkOrMountCustomLocation tries to check or mount the BPF filesystem in the
// given path.
func cgrpCheckOrMountLocation(cgroupRoot string) error {
	setCgroupRoot(cgroupRoot)

	// Check whether the custom location has a mount.
	mounted, cgroupInstance, err := mountinfo.IsMountFS(mountinfo.FilesystemTypeCgroup2, cgroupRoot)
	if err != nil {
		return err
	}

	// If the custom location has no mount, let's mount there.
	if !mounted {
		if err := mountCgroup(); err != nil {
			return err
		}
	}

	if !cgroupInstance {
		return fmt.Errorf("Mount in the custom directory %s has a different filesystem than cgroup2", cgroupRoot)
	}
	return nil
}

// CheckOrMountCgrpFS this checks if the cilium cgroup2 root mount point is
// mounted and if not mounts it. If mapRoot is "" it will mount the default
// location. It is harmless to have multiple cgroupv2 root mounts so unlike
// BPFFS case we simply mount at the cilium default regardless if the system
// has another mount created by systemd or otherwise.
func CheckOrMountCgrpFS(mapRoot string) {
	cgrpMountOnce.Do(func() {
		if mapRoot == "" {
			mapRoot = cgroupRoot
		}
		err := cgrpCheckOrMountLocation(mapRoot)
		// Failed cgroup2 mount is not a fatal error, sockmap will be disabled however
		if err == nil {
			log.Infof("Mounted Cgroup2 filesystem %s", mapRoot)
		}
	})
}

// BPF programs and sockmaps working on cgroups
func bpftoolMapAttach(progID string, mapID string, attachType string) error {
	prog := "bpftool"

	args := []string{"prog", "attach", "id", progID, attachType, "id", mapID}
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("Map Attach BPF Object:")
	_, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to attach prog(%s) to map(%s): %s", progID, mapID, err)
	}
	return nil
}

// #bpftool cgroup attach $cgrp sock_ops /sys/fs/bpf/$bpfObject
func bpftoolAttach(bpfObject string) error {
	prog := "bpftool"
	bpffs := bpf.GetMapRoot() + "/" + bpfObject
	cgrp := cgroupRoot //+ "/system.slice/docker.service"

	args := []string{"cgroup", "attach", cgrp, "sock_ops", "pinned", bpffs}
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("Attach BPF Object:")
	_, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to attach %s: %s", bpfObject, err)
	}
	return nil
}

// #bpftool cgroup detach $cgrp sock_ops /sys/fs/bpf/$bpfObject
func bpftoolDetach(bpfObject string) error {
	prog := "bpftool"
	bpffs := bpf.GetMapRoot() + "/" + bpfObject
	cgrp := cgroupRoot //+ "/system.slice/docker.service"

	args := []string{"cgroup", "detach", cgrp, "sock_ops", "pinned", bpffs}
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("Detach BPF Object:")
	_, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to detach %s: %s", bpfObject, err)
	}
	return nil

}

// #bpftool prog load $bpfObject /sys/fs/bpf/sockops
func bpftoolLoad(bpfObject string, bpfFsFile string) error {
	sockopsMaps := [...]string{
		"cilium_lxc",
		"cilium_ipcache",
		"cilium_metric",
		"cilium_events",
		"sock_ops_map",
		"sock_ops_ktls_up",
		"sock_ops_ktls_down",
		"cilium_ep_to_policy",
		"cilium_proxy4", "cilium_proxy6",
		"cilium_lb6_reverse_nat", "cilium_lb4_reverse_nat",
		"cilium_lb6_services", "cilium_lb4_services",
		"cilium_lb6_rr_seq", "cilium_lb4_seq",
		"cilium_lb6_rr_seq", "cilium_lb4_seq",
	}

	prog := "bpftool"
	var mapArgList []string
	bpffs := bpf.GetMapRoot() + "/" + bpfFsFile

	maps, err := ioutil.ReadDir(bpf.GetMapRoot() + "/tc/globals/")
	if err != nil {
		return err
	}

	for _, f := range maps {
		// Ignore all backing files
		if strings.HasPrefix(f.Name(), "..") {
			continue
		}

		use := func() bool {
			for _, n := range sockopsMaps {
				if f.Name() == n {
					return true
				}
			}
			return false
		}()

		if !use {
			continue
		}

		mapString := []string{"map", "name", f.Name(), "pinned", bpf.GetMapRoot() + "/tc/globals/" + f.Name()}
		mapArgList = append(mapArgList, mapString...)
	}

	args := []string{"-m", "prog", "load", bpfObject, bpffs}
	args = append(args, mapArgList...)
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("Load BPF Object:")
	_, err = exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to load %s: %s", bpfObject, err)
	}
	return nil
}

// #rm $bpfObject
func bpftoolUnload(bpfObject string) {
	bpffs := bpf.GetMapRoot() + "/" + bpfObject

	os.Remove(bpffs)
}

// #bpftool prog show pinned /sys/fs/bpf/
func bpftoolGetProgID(progName string) (string, error) {
	bpffs := bpf.GetMapRoot() + "/" + progName
	prog := "bpftool"

	args := []string{"prog", "show", "pinned", bpffs}
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("GetProgID:")
	output, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Failed to load %s: %s", progName, err)
	}

	// Scrap the prog_id out of the bpftool output after libbpf is dual licensed
	// we will use programatic API.
	s := strings.Fields(string(output))
	if s[0] == "" {
		return "", fmt.Errorf("Failed to find prog %s: %s", progName, err)
	}
	progID := strings.Split(s[0], ":")
	return progID[0], nil
}

// #bpftool prog show pinned /sys/fs/bpf/bpf_sockops
// #bpftool map show id 21
func bpftoolGetMapID(progName string, mapName string) (int, error) {
	bpffs := bpf.GetMapRoot() + "/" + progName
	prog := "bpftool"

	args := []string{"prog", "show", "pinned", bpffs}
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("GetMapID:")
	output, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("Failed to load %s: %s", progName, err)
	}

	// Find the mapID out of the bpftool output
	s := strings.Fields(string(output))
	for i := range s {
		if s[i] == "map_ids" {
			id := strings.Split(s[i+1], ",")
			for j := range id {
				args := []string{"map", "show", "id", id[j]}
				output, err := exec.Command(prog, args...).CombinedOutput()
				if err != nil {
					return 0, err
				}
				log.Debugf("mapid(%s): %s", mapName, output)

				if strings.Contains(string(output), mapName) {
					mapID, _ := strconv.Atoi(id[j])
					return mapID, nil
				}
			}
			break
		}
	}
	return 0, nil
}

// #bpftool map pin id map_id /sys/fs/bpf/tc/globals
func bpftoolPinMapID(mapName string, mapID int) error {
	bpffs := bpf.GetMapRoot()
	globals := bpffs + "/" + mapPrefix + "/"
	mapFile := globals + mapName
	prog := "bpftool"

	args := []string{"map", "pin", "id", strconv.Itoa(mapID), mapFile}
	log.WithFields(logrus.Fields{
		"bpftool": prog,
		"args":    args,
	}).Debug("Map pin:")
	_, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to pin map %d(%s): %s", mapID, mapName, err)
	}

	return nil
}

// #clang ... | llc ...
func bpfCompileProg(src string, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()

	srcpath := filepath.Join("sockops", src)
	outpath := filepath.Join(dst)

	err := loader.Compile(ctx, srcpath, outpath)
	if err != nil {
		return fmt.Errorf("failed compile %s: %s", srcpath, err)
	}
	return nil
}

func bpfLoadMapProg(object string, load string, sockMap string, attachType string) error {
	var _mapID int

	sockops := object
	sockopsObj := option.Config.StateDir + "/" + sockops
	sockopsLoad := load

	err := bpftoolLoad(sockopsObj, sockopsLoad)
	if err != nil {
		return err
	}

	progID, err := bpftoolGetProgID(load)
	if err != nil {
		return err
	}

	// Todo for some reason names are not being attached to
	// ktls maps so we use this trick to find them for now.
	if sockMap == "ingress" {
		_mapID, err = bpftoolGetMapID("bpf_redir_ing", "sockmap")
	} else if sockMap == "egress" {
		_mapID, err = bpftoolGetMapID("bpf_redir", "sockmap")
	} else {
		_mapID, err = bpftoolGetMapID("bpf_redir", sockMap)
	}
	mapID := strconv.Itoa(_mapID)
	if err != nil {
		return err
	}

	err = bpftoolMapAttach(progID, mapID, attachType)
	if err != nil {
		return err
	}
	return nil
}

// KtlsEnable will compile and attach the SK_MSG programs to the
// sockmap used to redirect to/from a Ktls enabled proxy. After
// this all kTLS traffic (as identified by policy map) will be sent
// to the user space proxy for handling before encryption.
func KtlsEnable() error {
	err := bpfCompileProg(cKtlsUp, oKtlsUp)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfCompileProg(cKtlsDown, oKtlsDown)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfLoadMapProg(oKtlsUp, eKtlsUp, "egress", msgVerdict)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfLoadMapProg(oKtlsDown, eKtlsDown, "ingress", msgVerdict)
	if err != nil {
		log.Error(err)
		return err
	}

	log.Info("kTLS sockmsg Enabled, bpf_ktls loaded")
	return nil
}

// KtlsDisable "unloads" the SK_MSG program associated with the
// kTLS proxy. This simply deletes the file associated with the program.
func KtlsDisable() {
	bpftoolUnload(eKtlsUp)
	bpftoolUnload(eKtlsDown)
	log.Info("Ktls sockmsg Disabled.")
}

// SkmsgEnable will compile and attach the SK_MSG programs to the
// sockmap. After this all sockets added to the sock_ops_map will
// have sendmsg/sendfile calls running through BPF program.
func SkmsgEnable() error {
	err := bpfCompileProg(cIPC, oIPC)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfCompileProg(cskbIPC, oskbIPC)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfCompileProg(cparserIPC, oparserIPC)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfLoadMapProg(oIPC, eIPC, sockMap, msgVerdict)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfLoadMapProg(oskbIPC, eskbIPC, sockMap, skbVerdict)
	if err != nil {
		log.Error(err)
		return err
	}

	err = bpfLoadMapProg(oparserIPC, eparserIPC, sockMap, skbParser)
	if err != nil {
		log.Error(err)
		return err
	}

	log.Info("Sockmsg Enabled, bpf_redir loaded")
	return nil
}

// SkmsgDisable "unloads" the SK_MSG program. This simply deletes
// the file associated with the program.
func SkmsgDisable() {
	bpftoolUnload(eIPC)
	bpftoolUnload(eskbIPC)
	bpftoolUnload(eparserIPC)
	log.Info("Sockmsg Disabled.")
}

// First user of sockops root is sockops load programs so we ensure the sockops
// root path no longer changes.
func bpfLoadAttachProg(object string, load string, mapName string) (int, int, error) {
	sockopsObj := option.Config.StateDir + "/" + object
	mapID := 0

	err := bpftoolLoad(sockopsObj, load)
	if err != nil {
		return 0, 0, err
	}
	err = bpftoolAttach(load)
	if err != nil {
		return 0, 0, err
	}

	if mapName != "" {
		mapID, err = bpftoolGetMapID(load, mapName)
		if err != nil {
			return 0, mapID, err
		}

		err = bpftoolPinMapID(mapName, mapID)
		if err != nil {
			return 0, mapID, err
		}
	}
	return 0, mapID, nil
}

// SockmapEnable will compile sockops programs and attach the sockops programs
// to the cgroup. After this all TCP connect events will be filtered by a BPF
// sockops program.
func SockmapEnable() error {
	err := bpfCompileProg(cSockops, oSockops)
	if err != nil {
		log.Error(err)
		return err
	}
	progID, mapID, err := bpfLoadAttachProg(oSockops, eSockops, sockMap)
	if err != nil {
		log.Error(err)
		return err
	}
	log.Infof("Sockmap Enabled: bpf_sockops prog_id %d and map_id %d loaded", progID, mapID)
	return nil
}

// SockmapDisable will detach any sockmap programs from cgroups then "unload"
// all the programs and maps associated with it. Here "unload" just means
// deleting the file associated with the map.
func SockmapDisable() {
	mapName := mapPrefix + "/" + sockMap
	bpftoolDetach(eSockops)
	bpftoolUnload(eSockops)
	bpftoolUnload(mapName)
	log.Info("Sockmap disabled.")
}

func SockmapKtlsDisable() {
	downMapName := mapPrefix + "/" + sockKtlsDownMap
	upMapName := mapPrefix + "/" + sockKtlsUpMap
	bpftoolUnload(eKtlsUp)
	bpftoolUnload(eKtlsDown)
	bpftoolUnload(downMapName)
	bpftoolUnload(upMapName)
	log.Info("kTLS disabled.")

}
