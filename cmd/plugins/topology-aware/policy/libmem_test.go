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
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"runtime"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	m "github.com/ozhuraki/gofmbt/gofmbt"
	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/resmgr/policy/topologyaware"
	policyapi "github.com/containers/nri-plugins/pkg/resmgr/policy"
	system "github.com/containers/nri-plugins/pkg/sysfs"
	"github.com/containers/nri-plugins/pkg/utils"
	"k8s.io/klog/v2"
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

// LibmemState is the abstract model state for TestLibmemGofmbt2. It
// tracks how many bytes are free and which named allocations are live.
type LibmemState struct {
	freeBytes int64
	allocs    map[string]int64 // abstract name -> allocated size
}

// setupTestPolicy creates a policy from the server sysfs testdata.
// If testdata/sysfs/server/sys already exists in the current directory it is
// used directly and the returned dir is empty (caller must not delete it).
// Otherwise the tarball is unpacked into a temp dir and that dir is returned
// so the caller can clean it up with removeAll.
func setupTestPolicy(t *testing.T) (*policy, string) {
	t.Helper()

	const preUnpacked = "testdata/sysfs/server/sys"
	var sysPath string
	var dir string

	if _, err := os.Stat(preUnpacked); err == nil {
		sysPath = preUnpacked
	} else {
		var err error
		dir, err = os.MkdirTemp("", "nri-libmem-test-")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		if err := utils.UncompressTbz2(path.Join("testdata", "sysfs.tar.bz2"), dir); err != nil {
			if rerr := os.RemoveAll(dir); rerr != nil {
				t.Logf("failed to remove temp dir %q: %v", dir, rerr)
			}
			t.Fatalf("failed to uncompress testdata: %v", err)
		}
		sysPath = path.Join(dir, "sysfs", "server", "sys")
	}

	sys, err := system.DiscoverSystemAt(sysPath)
	if err != nil {
		if dir != "" {
			if rerr := os.RemoveAll(dir); rerr != nil {
				t.Logf("failed to remove temp dir %q: %v", dir, rerr)
			}
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
		if dir != "" {
			if rerr := os.RemoveAll(dir); rerr != nil {
				t.Logf("failed to remove temp dir %q: %v", dir, rerr)
			}
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

// mallocSeq is used to generate unique container IDs in malloc.
var mallocSeq int

// malloc allocates memory of the given size on a leaf DRAM node of the policy
// and returns the container ID of the committed allocation.
func malloc(p *policy, size int64) (string, error) {
	mallocSeq++
	id := fmt.Sprintf("test-container-%d", mallocSeq)

	fmt.Printf("malloc: id=%s size=%d\n", id, size)
	for i := 1; i <= callerDepth; i++ {
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		fmt.Printf("  [%d] %s (%s:%d)\n", i, runtime.FuncForPC(pc).Name(), file, line)
	}

	var pool Node
	for _, n := range p.pools {
		if n.IsLeafNode() && n.HasMemoryType(memoryDRAM) {
			pool = n
			break
		}
	}
	if pool == nil {
		return "", fmt.Errorf("no leaf DRAM node found in test system")
	}
	ctr := &mockContainer{returnValueForGetID: id}
	req := &request{
		memType:   memoryDRAM,
		memReq:    size,
		container: ctr,
	}
	offer, err := p.getMemOffer(pool, req)
	if err != nil {
		return "", fmt.Errorf("getMemOffer failed: %w", err)
	}
	if _, _, err := offer.Commit(); err != nil {
		return "", fmt.Errorf("Offer.Commit() failed: %w", err)
	}
	return id, nil
}

// free releases a previously committed memory allocation for the given container ID.
func free(p *policy, id string) error {
	fmt.Printf("free: id=%s\n", id)
	for i := 1; i <= callerDepth; i++ {
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		fmt.Printf("  [%d] %s (%s:%d)\n", i, runtime.FuncForPC(pc).Name(), file, line)
	}
	return p.releaseMem(id)
}

// TestLibmemReleaseMem verifies that releaseMem releases a previously committed
// memory allocation, and returns an error for an unknown ID.
func TestLibmemReleaseMem(t *testing.T) {
	p, dir := setupTestPolicy(t)
	defer removeAll(t, dir)

	id, err := malloc(p, 64*1024*1024) // 64 MiB
	if err != nil {
		t.Fatalf("malloc failed: %v", err)
	}

	if err := free(p, id); err != nil {
		t.Errorf("free failed for known ID: %v", err)
	}

	// Releasing the same ID again should return an error (unknown request).
	if err := free(p, id); err == nil {
		t.Error("expected error releasing unknown ID, got nil")
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

func (s *LibmemState) String() string {
	names := make([]string, 0, len(s.allocs))
	for name := range s.allocs {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("[free:%dMiB allocs:[%s]]", s.freeBytes>>20, strings.Join(names, " "))
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
			m.OnAction("NAME=be0 CONTCOUNT=1 CPU=0 MEM=0 create besteffort").Register(createPod, "be0", 1*0, 0, 1*0).Do(createPod("be0", 1*0, 0, 1*0)),
			m.OnAction("NAME=be1 CONTCOUNT=3 CPU=0 MEM=0 create besteffort").Register(createPod, "be1", 3*0, 0, 3*0).Do(createPod("be1", 3*0, 0, 3*0)),
			m.OnAction("NAME=rbe0 CONTCOUNT=2 CPU=0 MEM=0 namespace=kube-system create besteffort").Register(createPod, "rbe0", 0, 2*0, 2*0).Do(createPod("rbe0", 0, 2*0, 2*0)),
			m.When(s.mem > 0,
				m.When(s.cpu >= 200,
					m.OnAction("NAME=gu0 CONTCOUNT=1 CPU=200m MEM=1500M create guaranteed").Register(createPod, "gu0", 1*200, 0, 1*1500).Do(createPod("gu0", 1*200, 0, 1*1500)),
					m.OnAction("NAME=gu1 CONTCOUNT=2 CPU=1000m MEM=500M create guaranteed").Register(createPod, "gu1", 2*1000, 0, 2*500).Do(createPod("gu1", 2*1000, 0, 2*500)),
					m.OnAction("NAME=gu2 CONTCOUNT=2 CPU=1200m MEM=4500M create guaranteed").Register(createPod, "gu2", 2*1200, 0, 2*4500).Do(createPod("gu2", 2*1200, 0, 2*4500)),
					m.OnAction("NAME=gu3 CONTCOUNT=3 CPU=2000m MEM=500M create guaranteed").Register(createPod, "gu3", 3*2000, 0, 3*500).Do(createPod("gu3", 3*2000, 0, 3*500)),
					m.OnAction("NAME=gu4 CONTCOUNT=1 CPU=4200m MEM=100M create guaranteed").Register(createPod, "gu4", 1*4200, 0, 1*100).Do(createPod("gu4", 1*4200, 0, 1*100)),
					m.OnAction("NAME=bu0 CONTCOUNT=1 CPU=1200m MEM=50M CPUREQ=900m MEMREQ=49M CPULIM=1200m MEMLIM=50M create burstable").Register(createPod, "bu0", 1*1200, 0, 1*50).Do(createPod("bu0", 1*1200, 0, 1*50)),
					m.OnAction("NAME=bu1 CONTCOUNT=2 CPU=1900m MEM=300M CPUREQ=1800m MEMREQ=299M CPULIM=1900m MEMLIM=300M create burstable").Register(createPod, "bu1", 2*1900, 0, 2*300).Do(createPod("bu1", 2*1900, 0, 2*300)),
				),
				m.When(s.rcpu > 99,
					m.OnAction("NAME=rgu0 CONTCOUNT=2 CPU=100m MEM=1000M namespace=kube-system create guaranteed").Register(createPod, "rgu0", 0, 2*100, 2*1000).Do(createPod("rgu0", 0, 2*100, 2*1000)),
					m.OnAction("NAME=rbu0 CONTCOUNT=1 CPU=100m MEM=100M CPUREQ=99m MEMREQ=99M CPULIM=100m MEMLIM=100M namespace=kube-system create burstable").Register(createPod, "rbu0", 0, 1*100, 1*100).Do(createPod("rbu0", 0, 1*100, 1*100)),
				),
			),
		)
	})

	model.From(func(current m.State) []*m.Transition {
		s := current.(*TestState)
		ts := []*m.Transition{}
		for _, pod := range podNames {
			if _, ok := s.podRes[pod]; ok {
				ts = append(ts, m.OnAction("NAME=%s vm-command 'kubectl delete pod %s --now'", pod, pod).Register(deletePod, pod).Do(deletePod(pod))...)
			}
		}
		for _, pod := range rPodNames {
			if _, ok := s.podRes[pod]; ok {
				ts = append(ts, m.OnAction("NAME=%s vm-command 'kubectl delete pod --namespace kube-system %s --now'", pod, pod).Register(deletePod, pod).Do(deletePod(pod))...)
			}
		}
		return ts
	})

	return model
}

var (
	maxMem         int
	maxCpu         int
	maxReservedCpu int
	maxTestSteps   int
	randomSeed     int64
	randomness     int
	searchDepth    int

	maxLibmem2Steps int
	libmem2Search   int
	callerDepth     int
)

// init switches CommandLine to ContinueOnError so that the test framework's
// first flag.Parse() tolerates flags registered later inside test functions
// (e.g. -caller-depth), allowing them to be passed from the command line.
func init() {
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
}

func TestLibmemGofmbt(t *testing.T) {
	flag.IntVar(&maxMem, "mem", 7500, "memory available for test pods")
	flag.IntVar(&maxCpu, "cpu", 15000, "non-reserved milli-CPU available for test pods")
	flag.IntVar(&maxReservedCpu, "reserved-cpu", 1000, "reserved milli-CPU availble for test pods")
	flag.IntVar(&maxTestSteps, "test-steps", 3000, "number of test steps")
	flag.Int64Var(&randomSeed, "random-seed", 0, "random seed for selecting best path")
	flag.IntVar(&randomness, "randomness", 0, "the greater the randomness, the larger the set of paths for choosing best path. 0 means no randomness, 5 picks any path that increases coverage.")
	flag.IntVar(&searchDepth, "search-depth", 4, "number of steps to look ahead when selecting best path")

	flag.Parse()

	_, generateGoFile, _, _ := runtime.Caller(0)

	model := newModel()
	coverer := m.NewCoverer()
	coverer.CoverActionCombinations(3)

	if randomSeed > 0 || randomness > 0 {
		coverer.SetBestPathRandom(randomSeed, randomness)
	}

	var state m.State

	state = &TestState{
		cpu:  maxCpu,
		rcpu: maxReservedCpu,
		mem:  maxMem,
	}
	fmt.Printf("echo === generated by: %s --mem=%d --cpu=%d --reserved-cpu=%d --test-steps=%d --random-seed=%d --randomness=%d --search-depth=%d\n",
		generateGoFile, maxMem, maxCpu, maxReservedCpu, maxTestSteps, randomSeed, randomness, searchDepth)
	testStep := 0
	for testStep < maxTestSteps {
		path, covStats := coverer.BestPath(model, state, searchDepth)
		if len(path) == 0 {
			fmt.Printf("# did not find anything to cover\n")
			break
		}
		for i := 0; i < covStats.MaxStep+1; i++ {
			testStep++
			step := path[i]
			fmt.Printf("\necho === step:%d coverage:%d state:%v\n", testStep, coverer.Coverage(), state)
			fmt.Println(step.Action())
			state = step.EndState()
			coverer.MarkCovered(step)
			coverer.UpdateCoverage()
			if testStep >= maxTestSteps {
				break
			}
		}
	}

}

// TestLibmemGofmbt2 uses gofmbt model-based testing to drive malloc/free
// sequences against the policy, verifying that all operations succeed.
func TestLibmemGofmbt2(t *testing.T) {
	flag.IntVar(&maxLibmem2Steps, "libmem2-steps", 1000, "number of test steps for TestLibmemGofmbt2")
	flag.IntVar(&libmem2Search, "libmem2-search-depth", 4, "look-ahead depth for TestLibmemGofmbt2")
	flag.IntVar(&callerDepth, "caller-depth", 1, "number of caller frames printed by malloc() and free()")

	flag.Parse()

	klog.SetLogger(logr.Discard())
	p, dir := setupTestPolicy(t)
	klog.ClearLogger()
	defer removeAll(t, dir)

	allocNames := []string{"a0", "a1", "a2", "a3", "a4"}
	allocSizes := map[string]int64{
		"a0": 64 << 20,
		"a1": 128 << 20,
		"a2": 64 << 20,
		"a3": 32 << 20,
		"a4": 32 << 20,
	}

	var totalAllocBytes int64
	for _, size := range allocSizes {
		totalAllocBytes += size
	}

	allocIDs := map[string]string{} // abstract name -> real container ID
	var execute bool                 // true only during step execution; guards doMalloc/doFree from BestPath exploration calls

	doMalloc := func(name string) (string, error) {
		if !execute {
			return "", nil
		}
		id, err := malloc(p, allocSizes[name])
		if err == nil {
			allocIDs[name] = id
		}
		return id, err
	}

	doFree := func(name string) error {
		if !execute {
			return nil
		}
		id, ok := allocIDs[name]
		if !ok {
			return nil
		}
		err := free(p, id)
		if err == nil {
			delete(allocIDs, name)
		}
		return err
	}

	mallocFn := func(name string, size int64) m.StateChange {
		return func(curr m.State) m.State {
			s := curr.(*LibmemState)
			if _, ok := s.allocs[name]; ok || s.freeBytes < size {
				return nil
			}
			newAllocs := make(map[string]int64, len(s.allocs)+1)
			for k, v := range s.allocs {
				newAllocs[k] = v
			}
			newAllocs[name] = size
			pc, _, _, _ := runtime.Caller(1)
			fmt.Printf("mallocFn %s (%d MiB) called from %s\n", name, size>>20, runtime.FuncForPC(pc).Name())
			return &LibmemState{freeBytes: s.freeBytes - size, allocs: newAllocs}
		}
	}

	freeFn := func(name string) m.StateChange {
		return func(curr m.State) m.State {
			s := curr.(*LibmemState)
			size, ok := s.allocs[name]
			if !ok {
				return nil
			}
			newAllocs := make(map[string]int64, len(s.allocs))
			for k, v := range s.allocs {
				if k != name {
					newAllocs[k] = v
				}
			}
			pc, _, _, _ := runtime.Caller(1)
			fmt.Printf("freeFn %s (%d MiB) called from %s\n", name, size>>20, runtime.FuncForPC(pc).Name())
			return &LibmemState{freeBytes: s.freeBytes + size, allocs: newAllocs}
		}
	}

	model := m.NewModel()

	model.From(func(curr m.State) []*m.Transition {
		s := curr.(*LibmemState)
		var ts []*m.Transition
		for _, name := range allocNames {
			if _, ok := s.allocs[name]; !ok && s.freeBytes >= allocSizes[name] {
				ts = append(ts, m.OnAction("malloc %s", name).Register(doMalloc, name).Do(mallocFn(name, allocSizes[name]))...)
			}
		}
		return ts
	})

	model.From(func(curr m.State) []*m.Transition {
		s := curr.(*LibmemState)
		var ts []*m.Transition
		for _, name := range allocNames {
			if _, ok := s.allocs[name]; ok {
				ts = append(ts, m.OnAction("free %s", name).Register(doFree, name).Do(freeFn(name))...)
			}
		}
		return ts
	})

	coverer := m.NewCoverer()
	coverer.CoverActionCombinations(3)

	state := m.State(&LibmemState{
		freeBytes: totalAllocBytes,
		allocs:    map[string]int64{},
	})

	testStep := 0
	for testStep < maxLibmem2Steps {
		path, covStats := coverer.BestPath(model, state, libmem2Search)
		if len(path) == 0 {
			break
		}
		for i := 0; i <= covStats.MaxStep; i++ {
			testStep++
			step := path[i]
			fmt.Printf("\necho === step:%d coverage:%d state:%v\n", testStep, coverer.Coverage(), state)
			fmt.Println(step.Action())
			execute = true
			results := step.Action().Execute()
			execute = false
			if len(results) > 0 {
				if err, _ := results[len(results)-1].(error); err != nil {
					t.Errorf("step %d: %s failed: %v", testStep, step.Action(), err)
				}
			}
			state = step.EndState()
			coverer.MarkCovered(step)
			coverer.UpdateCoverage()
			if testStep >= maxLibmem2Steps {
				break
			}
		}
	}
}
