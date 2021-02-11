// Copyright 2019-2021 The Inspektor Gadget authors
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

package gadgettracermanager

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/cilium/ebpf"

	"golang.org/x/sys/unix"

	pb "github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/api"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/k8s"
)

const (
	PIN_PATH         = "/sys/fs/bpf/gadget"
	MNTMAP_PREFIX    = "mntnsset_"
	CGROUPMAP_PREFIX = "cgroupidset_"
)

type GadgetTracerManager struct {
	mu sync.Mutex

	// node where this instance is running
	nodeName string

	// client to talk to the k8s API server to get information about pods
	k8sClient *k8s.K8sClient

	// containers by ContainerId
	containers map[string]pb.ContainerDefinition

	// tracers by tracerId
	tracers map[string]tracer

	podInformer *k8s.PodInformer
	createdChan chan *v1.Pod
	deletedChan chan string
	// containerIDsByKey is a map maintained by the controller
	// key is "namespace/podname"
	// value is an set of containerId
	containerIDsByKey map[string]map[string]struct{}
}

type tracer struct {
	tracerId string

	containerSelector pb.ContainerSelector

	cgroupIdSetMap *ebpf.Map
	mntnsSetMap    *ebpf.Map
}

func containerSelectorMatches(s *pb.ContainerSelector, c *pb.ContainerDefinition) bool {
	if s.Namespace != "" && s.Namespace != c.Namespace {
		return false
	}
	if s.Podname != "" && s.Podname != c.Podname {
		return false
	}
	if s.ContainerName != "" && s.ContainerName != c.ContainerName {
		return false
	}
	for _, l := range s.Labels {
		found := false
		for _, cl := range c.Labels {
			if cl.Key == l.Key && cl.Value == l.Value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func (g *GadgetTracerManager) AddTracer(ctx context.Context, req *pb.AddTracerRequest) (*pb.TracerID, error) {
	tracerId := ""
	if req.Id == "" {
		b := make([]byte, 6)
		_, err := rand.Read(b)
		if err != nil {
			return nil, fmt.Errorf("cannot generate random number: %v", err)
		}
		tracerId = fmt.Sprintf("%x", b)
	} else {
		tracerId = req.Id
	}
	if _, ok := g.tracers[tracerId]; ok {
		return nil, fmt.Errorf("tracer id %q already exists", tracerId)
	}

	// Create and pin BPF maps for this tracer.
	cgroupIdSpec := &ebpf.MapSpec{
		Name:       CGROUPMAP_PREFIX + tracerId,
		Type:       ebpf.Hash,
		KeySize:    8,
		ValueSize:  4,
		MaxEntries: 128,
		Pinning:    ebpf.PinByName,
	}
	cgroupIdSetMap, err := ebpf.NewMapWithOptions(cgroupIdSpec, ebpf.MapOptions{PinPath: PIN_PATH})
	if err != nil {
		return nil, fmt.Errorf("error creating cgroupid map: %w", err)
	}

	mntnsSpec := &ebpf.MapSpec{
		Name:       MNTMAP_PREFIX + tracerId,
		Type:       ebpf.Hash,
		KeySize:    8,
		ValueSize:  4,
		MaxEntries: 10240,
		Pinning:    ebpf.PinByName,
	}
	mntnsSetMap, err := ebpf.NewMapWithOptions(mntnsSpec, ebpf.MapOptions{PinPath: PIN_PATH})
	if err != nil {
		return nil, fmt.Errorf("error creating mntnsset map: %w", err)
	}

	for _, c := range g.containers {
		if containerSelectorMatches(req.Selector, &c) {
			one := uint32(1)
			cgroupIdC := uint64(c.CgroupId)
			if cgroupIdC != 0 {
				cgroupIdSetMap.Put(cgroupIdC, one)
			}
			mntnsC := uint64(c.Mntns)
			if mntnsC != 0 {
				mntnsSetMap.Put(mntnsC, one)
			}
		}
	}

	g.tracers[tracerId] = tracer{
		tracerId:          tracerId,
		containerSelector: *req.Selector,
		cgroupIdSetMap:    cgroupIdSetMap,
		mntnsSetMap:       mntnsSetMap,
	}
	return &pb.TracerID{Id: tracerId}, nil
}

func (g *GadgetTracerManager) RemoveTracer(ctx context.Context, tracerID *pb.TracerID) (*pb.RemoveTracerResponse, error) {
	if tracerID.Id == "" {
		return nil, fmt.Errorf("cannot remove tracer: Id not set")
	}

	t, ok := g.tracers[tracerID.Id]
	if !ok {
		return nil, fmt.Errorf("cannot remove tracer: unknown tracer %q", tracerID.Id)
	}

	t.cgroupIdSetMap.Close()
	t.mntnsSetMap.Close()

	os.Remove(filepath.Join(PIN_PATH, CGROUPMAP_PREFIX+t.tracerId))
	os.Remove(filepath.Join(PIN_PATH, MNTMAP_PREFIX+t.tracerId))

	delete(g.tracers, tracerID.Id)
	return &pb.RemoveTracerResponse{}, nil
}

func (g *GadgetTracerManager) AddContainer(ctx context.Context, containerDefinition *pb.ContainerDefinition) (*pb.AddContainerResponse, error) {
	if containerDefinition.ContainerId == "" {
		return nil, fmt.Errorf("cannot add container: container id not set")
	}
	if _, ok := g.containers[containerDefinition.ContainerId]; ok {
		return nil, fmt.Errorf("container with id %s already exists", containerDefinition.ContainerId)
	}

	// If the pod name isn't provided, use k8s API server to get the
	// missing information about the container.
	if containerDefinition.Podname == "" {
		if err := g.k8sClient.FillContainer(containerDefinition); err != nil {
			return nil, err
		}
	}

	for _, t := range g.tracers {
		if containerSelectorMatches(&t.containerSelector, containerDefinition) {
			cgroupIdC := uint64(containerDefinition.CgroupId)
			mntnsC := uint64(containerDefinition.Mntns)
			one := uint32(1)
			if cgroupIdC != 0 {
				t.cgroupIdSetMap.Put(cgroupIdC, one)
			}
			if mntnsC != 0 {
				t.mntnsSetMap.Put(mntnsC, one)
			}
		}
	}

	g.containers[containerDefinition.ContainerId] = *containerDefinition
	return &pb.AddContainerResponse{}, nil
}

func (g *GadgetTracerManager) RemoveContainer(ctx context.Context, containerDefinition *pb.ContainerDefinition) (*pb.RemoveContainerResponse, error) {
	if containerDefinition.ContainerId == "" {
		return nil, fmt.Errorf("cannot remove container: ContainerId not set")
	}

	c, ok := g.containers[containerDefinition.ContainerId]
	if !ok {
		return nil, fmt.Errorf("cannot remove container: unknown container %q", containerDefinition.ContainerId)
	}

	for _, t := range g.tracers {
		if containerSelectorMatches(&t.containerSelector, &c) {
			cgroupIdC := uint64(c.CgroupId)
			mntnsC := uint64(c.Mntns)
			t.cgroupIdSetMap.Delete(cgroupIdC)
			t.mntnsSetMap.Delete(mntnsC)
		}
	}

	delete(g.containers, containerDefinition.ContainerId)
	return &pb.RemoveContainerResponse{}, nil
}

func (g *GadgetTracerManager) DumpState(ctx context.Context, req *pb.DumpStateRequest) (*pb.Dump, error) {
	out := "List of containers:\n"
	for i, c := range g.containers {
		out += fmt.Sprintf("%v -> %+v\n", i, c)
	}
	out += "List of tracers:\n"
	for i, t := range g.tracers {
		out += fmt.Sprintf("%v -> %q/%q (%s) Labels: \n",
			i,
			t.containerSelector.Namespace,
			t.containerSelector.Podname,
			t.containerSelector.ContainerName)
		for _, l := range t.containerSelector.Labels {
			out += fmt.Sprintf("                  %v: %v\n", l.Key, l.Value)
		}
		out += fmt.Sprintf("        Matches:\n")
		for _, c := range g.containers {
			if containerSelectorMatches(&t.containerSelector, &c) {
				out += fmt.Sprintf("        - %s/%s [Mntns=%v CgroupId=%v]\n", c.Namespace, c.Podname, c.Mntns, c.CgroupId)
			}
		}
	}
	return &pb.Dump{State: out}, nil
}

func (g *GadgetTracerManager) run() {
	for {
		select {
		case d := <-g.deletedChan:
			if containerIDs, ok := g.containerIDsByKey[d]; ok {
				for containerID, _ := range containerIDs {
					containerDefinition := &pb.ContainerDefinition{
						ContainerId: containerID,
					}
					g.RemoveContainer(nil, containerDefinition)
				}
			}
		case c := <-g.createdChan:
			containers := g.k8sClient.PodToContainers(c)
			key, _ := cache.MetaNamespaceKeyFunc(c)
			containerIDs, ok := g.containerIDsByKey[key]
			if !ok {
				containerIDs = make(map[string]struct{})
				g.containerIDsByKey[key] = containerIDs
			}
			for _, container := range containers {
				// The container is already registered, there is not any chance the
				// PID will change, so ignore it.
				if _, ok := containerIDs[container.ContainerId]; ok {
					continue
				}

				g.AddContainer(nil, &container)
				containerIDs[container.ContainerId] = struct{}{}
			}
		}
	}
}

func NewServerWithPodInformer(nodeName string) (*GadgetTracerManager, error) {
	if err := initServer(); err != nil {
		return nil, err
	}

	createdChan := make(chan *v1.Pod)
	deletedChan := make(chan string)

	k8sClient, err := k8s.NewK8sClient(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	podInformer, err := k8s.NewPodInformer(nodeName, createdChan, deletedChan)
	if err != nil {
		return nil, fmt.Errorf("failed to create pod informer: %w", err)
	}
	g := &GadgetTracerManager{
		nodeName:          nodeName,
		containers:        make(map[string]pb.ContainerDefinition),
		tracers:           make(map[string]tracer),
		podInformer:       podInformer,
		createdChan:       createdChan,
		deletedChan:       deletedChan,
		containerIDsByKey: make(map[string]map[string]struct{}),
		k8sClient:         k8sClient,
	}

	go g.run()

	return g, nil
}

func NewServer(nodeName string) (*GadgetTracerManager, error) {
	if err := initServer(); err != nil {
		return nil, err
	}

	k8sClient, err := k8s.NewK8sClient(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}
	// The CRI client is only used at the beginning to get the initial list
	// of containers, it's not used after it.
	defer k8sClient.CloseCRI()

	g := &GadgetTracerManager{
		nodeName:   nodeName,
		containers: make(map[string]pb.ContainerDefinition),
		tracers:    make(map[string]tracer),
		k8sClient:  k8sClient,
	}

	containers, err := k8sClient.ListContainers()
	if err != nil {
		log.Printf("gadgettracermanager failed to list containers: %v", err)
	} else {
		log.Printf("gadgettracermanager found %d containers: %+v", len(containers), containers)
		for _, container := range containers {
			g.containers[container.ContainerId] = container
		}
	}
	return g, nil
}

func increaseRlimit() error {
	limit := &unix.Rlimit{
		Cur: unix.RLIM_INFINITY,
		Max: unix.RLIM_INFINITY,
	}
	return unix.Setrlimit(unix.RLIMIT_MEMLOCK, limit)
}

func initServer() error {
	if err := increaseRlimit(); err != nil {
		return fmt.Errorf("failed to increase limit memlock limit: %w", err)
	}

	if err := os.Mkdir(PIN_PATH, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("failed to create folder for pinning bpf maps: %w", err)
	}

	return nil
}
