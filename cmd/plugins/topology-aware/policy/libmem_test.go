// Copyright 2026 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"testing"

	m "github.com/ozhuraki/gofmbt/gofmbt"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
)

type PodResources struct {
	cpu  int // total CPU allocated for all containers in the pod
	rcpu int // total reserved CPU allocated for all containers in the pod
	mem  int // total memory allocated for all containers in the pod
}

type TestState struct {
	cpu    int                      // free CPU on node for non-reserved pods
	rcpu   int                      // free CPU on node for reserved pods
	mem    int                      // free memory on node
	podRes map[string]*PodResources // map running pod name to resources allocated to it
}

// setupTestPolicy creates a policy from the server sysfs testdata.
func setupTestPolicy(t *testing.T) (*policy, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nri-libmem-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Logf("failed to remove temp dir %q: %v", dir, rerr)
		}
		t.Fatalf("failed to uncompress testdata: %v", err)
	}

	sysPath := path.Join(dir, "sysfs", "server", "sys")
	sys, err := system.DiscoverSystemAt(sysPath)
	if err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Logf("failed to remove temp dir %q: %v", dir, rerr)
		}
		t.Fatalf("failed to discover system: %v", err)
	}

	p := New().(*policy)
	if err := p.Setup(&policyapi.BackendOptions{
		Cache:  &mockCache{},
		System: sys,
		Config: &cfgapi.Config{
			ReservedResources: cfgapi.Constraints{cfgapi.CPU: "750m"},
		},
	}); err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			t.Logf("failed to remove temp dir %q: %v", dir, rerr)
		}
		t.Fatalf("failed to setup policy: %v", err)
	}
	return p, dir
}

// TestLibmemGetMemOfferByHintsMemoryPreserve verifies that getMemOfferByHints
// returns an error immediately when memoryPreserve is requested.
func TestLibmemGetMemOfferByHintsMemoryPreserve(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	pool := p.pools[0]
	req := &request{
		memType:   memoryPreserve,
		container: &mockContainer{},
	}

	_, err := p.getMemOfferByHints(pool, req)
	if err == nil {
		t.Fatal("expected error for memoryPreserve, got nil")
	}
	if !strings.Contains(err.Error(), "memoryPreserve") {
		t.Errorf("expected 'memoryPreserve' in error, got: %v", err)
	}
}

// TestLibmemGetMemOfferByHintsNoHints verifies that getMemOfferByHints returns an
// error when the container has no pod resource API topology hints.
func TestLibmemGetMemOfferByHintsNoHints(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	// Find a leaf NUMA node with DRAM.
	var pool Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) {
			pool = n
			break
		}
	}
	if pool == nil {
		t.Fatal("no leaf DRAM node found in test system")
	}

	req := &request{
		memType:   memoryDRAM,
		container: &mockContainer{}, // GetTopologyHints() returns empty map
	}

	_, err := p.getMemOfferByHints(pool, req)
	if err == nil {
		t.Fatal("expected error when no hints provided, got nil")
	}
	if !strings.Contains(err.Error(), "no pod resource API hints") {
		t.Errorf("expected 'no pod resource API hints' in error, got: %v", err)
	}
}

// TestLibmemPoolZoneCapacityAndFree verifies that poolZoneCapacity returns a
// positive value and that poolZoneFree does not exceed it.
func TestLibmemPoolZoneCapacityAndFree(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	var pool Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) {
			pool = n
			break
		}
	}
	if pool == nil {
		t.Fatal("no leaf DRAM node found in test system")
	}

	capacity := p.poolZoneCapacity(pool, memoryDRAM)
	free := p.poolZoneFree(pool, memoryDRAM)

	if capacity <= 0 {
		t.Errorf("expected positive DRAM capacity, got %d", capacity)
	}
	if free < 0 || free > capacity {
		t.Errorf("expected 0 <= free (%d) <= capacity (%d)", free, capacity)
	}
}

func (s *TestState) String() string {
	pr := []string{}
	pods := make([]string, 0, len(s.podRes))
	for pod := range s.podRes {
		pods = append(pods, pod)
	}
	sort.Strings(pods)
	for _, pod := range pods {
		res := s.podRes[pod]
		switch {
		case res.rcpu == 0:
			pr = append(pr, fmt.Sprintf("%s:%dmCPU/%dM", pod, res.cpu, res.mem))
		case res.cpu == 0:
			pr = append(pr, fmt.Sprintf("%s:%dmRCPU/%dM", pod, res.rcpu, res.mem))
		default:
			pr = append(pr, fmt.Sprintf("%s:%dmCPU/%dmRCPU/%dM", pod, res.cpu, res.rcpu, res.mem))
		}
	}
	return fmt.Sprintf("[free:%dmCPU/%dmRCPU/%dM pods:[%s]]", s.cpu, s.rcpu, s.mem, strings.Join(pr, " "))
}

func createPod(pod string, cpu, rcpu, mem int) m.StateChange {
	return func(current m.State) m.State {
		s := current.(*TestState)
		if s.cpu < cpu || s.rcpu < rcpu || s.mem < mem {
			// refuse from state change if not enough resources
			return nil
		}
		if _, ok := s.podRes[pod]; ok {
			// refuse to create pod if it is already running
			return nil
		}
		newPodRes := make(map[string]*PodResources)
		for k, v := range s.podRes {
			newPodRes[k] = v
		}
		newPodRes[pod] = &PodResources{cpu, rcpu, mem}
		return &TestState{
			cpu:    s.cpu - cpu,
			rcpu:   s.rcpu - rcpu,
			mem:    s.mem - mem,
			podRes: newPodRes,
		}
	}
}

func deletePod(pod string) m.StateChange {
	return func(current m.State) m.State {
		s := current.(*TestState)
		res, ok := s.podRes[pod]
		if !ok {
			// refuse to delete pod if it is not running
			return nil
		}
		newPodRes := make(map[string]*PodResources)
		for k, v := range s.podRes {
			if k != pod {
				newPodRes[k] = v
			}
		}
		return &TestState{
			cpu:    s.cpu + res.cpu,
			rcpu:   s.rcpu + res.rcpu,
			mem:    s.mem + res.mem,
			podRes: newPodRes,
		}
	}
}

func newModel() *m.Model {
	podNames := []string{"gu0", "gu1", "gu2", "gu3", "gu4", "bu0", "bu1", "be0", "be1"}
	rPodNames := []string{"rbe0", "rgu0", "rbu0"}

	model := m.NewModel()

	model.From(func(current m.State) []*m.Transition {
		s := current.(*TestState)
		return m.When(true,
			m.OnAction("NAME=be0 CONTCOUNT=1 CPU=0 MEM=0 create besteffort").Do(createPod("be0", 1*0, 0, 1*0)),
			m.OnAction("NAME=be1 CONTCOUNT=3 CPU=0 MEM=0 create besteffort").Do(createPod("be1", 3*0, 0, 3*0)),
			m.OnAction("NAME=rbe0 CONTCOUNT=2 CPU=0 MEM=0 namespace=kube-system create besteffort").Do(createPod("rbe0", 0, 2*0, 2*0)),
			m.When(s.mem > 0,
				m.When(s.cpu >= 200,
					m.OnAction("NAME=gu0 CONTCOUNT=1 CPU=200m MEM=1500M create guaranteed").Do(createPod("gu0", 1*200, 0, 1*1500)),
					m.OnAction("NAME=gu1 CONTCOUNT=2 CPU=1000m MEM=500M create guaranteed").Do(createPod("gu1", 2*1000, 0, 2*500)),
					m.OnAction("NAME=gu2 CONTCOUNT=2 CPU=1200m MEM=4500M create guaranteed").Do(createPod("gu2", 2*1200, 0, 2*4500)),
					m.OnAction("NAME=gu3 CONTCOUNT=3 CPU=2000m MEM=500M create guaranteed").Do(createPod("gu3", 3*2000, 0, 3*500)),
					m.OnAction("NAME=gu4 CONTCOUNT=1 CPU=4200m MEM=100M create guaranteed").Do(createPod("gu4", 1*4200, 0, 1*100)),
					m.OnAction("NAME=bu0 CONTCOUNT=1 CPU=1200m MEM=50M CPUREQ=900m MEMREQ=49M CPULIM=1200m MEMLIM=50M create burstable").Do(createPod("bu0", 1*1200, 0, 1*50)),
					m.OnAction("NAME=bu1 CONTCOUNT=2 CPU=1900m MEM=300M CPUREQ=1800m MEMREQ=299M CPULIM=1900m MEMLIM=300M create burstable").Do(createPod("bu1", 2*1900, 0, 2*300)),
				),
				m.When(s.rcpu > 99,
					m.OnAction("NAME=rgu0 CONTCOUNT=2 CPU=100m MEM=1000M namespace=kube-system create guaranteed").Do(createPod("rgu0", 0, 2*100, 2*1000)),
					m.OnAction("NAME=rbu0 CONTCOUNT=1 CPU=100m MEM=100M CPUREQ=99m MEMREQ=99M CPULIM=100m MEMLIM=100M namespace=kube-system create burstable").Do(createPod("rbu0", 0, 1*100, 1*100)),
				),
			),
		)
	})

	model.From(func(current m.State) []*m.Transition {
		s := current.(*TestState)
		ts := []*m.Transition{}
		for _, pod := range podNames {
			if _, ok := s.podRes[pod]; ok {
				ts = append(ts, m.OnAction("NAME=%s vm-command 'kubectl delete pod %s --now'", pod, pod).Do(deletePod(pod))...)
			}
		}
		for _, pod := range rPodNames {
			if _, ok := s.podRes[pod]; ok {
				ts = append(ts, m.OnAction("NAME=%s vm-command 'kubectl delete pod --namespace kube-system %s --now'", pod, pod).Do(deletePod(pod))...)
			}
		}
		return ts
	})

	return model
}
