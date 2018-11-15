/*
Copyright 2015 The Kubernetes Authors.

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

package cache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/features"

	"github.com/golang/glog"
	policy "k8s.io/api/policy/v1beta1"
)

var (
	cleanAssumedPeriod = 1 * time.Second
)

// New returns a Cache implementation.
// It automatically starts a go routine that manages expiration of assumed pods.
// "ttl" is how long the assumed pod will get expired.
// "stop" is the channel that would close the background goroutine.
func New(ttl time.Duration, stop <-chan struct{}) Cache {
	cache := newSchedulerCache(ttl, cleanAssumedPeriod, stop)
	cache.run()
	return cache
}

type schedulerCache struct {
	stop   <-chan struct{}
	ttl    time.Duration
	period time.Duration

	// This mutex guards all fields within this cache struct.
	mu sync.Mutex
	// a set of assumed pod keys.
	// The key could further be used to get an entry in podStates.
	assumedPods map[string]bool
	// a map from pod key to podState.
	podStates map[string]*podState
	nodes     map[string]*NodeInfo
	pdbs      map[string]*policy.PodDisruptionBudget
}

type podState struct {
	pod *v1.Pod
	// Used by assumedPod to determinate expiration.
	deadline *time.Time
	// Used to block cache from expiring assumedPod if binding still runs
	bindingFinished bool
}

func newSchedulerCache(ttl, period time.Duration, stop <-chan struct{}) *schedulerCache {
	return &schedulerCache{
		ttl:    ttl,
		period: period,
		stop:   stop,

		nodes:       make(map[string]*NodeInfo),
		assumedPods: make(map[string]bool),
		podStates:   make(map[string]*podState),
		pdbs:        make(map[string]*policy.PodDisruptionBudget),
	}
}

// Snapshot takes a snapshot of the current schedulerCache. The method has performance impact,
// and should be only used in non-critical path.
func (cache *schedulerCache) Snapshot() *Snapshot {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	nodes := make(map[string]*NodeInfo)
	for k, v := range cache.nodes {
		nodes[k] = v.Clone()
	}

	assumedPods := make(map[string]bool)
	for k, v := range cache.assumedPods {
		assumedPods[k] = v
	}

	pdbs := make(map[string]*policy.PodDisruptionBudget)
	for k, v := range cache.pdbs {
		pdbs[k] = v.DeepCopy()
	}

	return &Snapshot{
		Nodes:       nodes,
		AssumedPods: assumedPods,
		Pdbs:        pdbs,
	}
}

func (cache *schedulerCache) UpdateNodeNameToInfoMap(nodeNameToInfo map[string]*NodeInfo) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for name, info := range cache.nodes {
		if utilfeature.DefaultFeatureGate.Enabled(features.BalanceAttachedNodeVolumes) && info.TransientInfo != nil {
			// Transient scheduler info is reset here.
			info.TransientInfo.resetTransientSchedulerInfo()
		}
		if current, ok := nodeNameToInfo[name]; !ok || current.generation != info.generation {
			nodeNameToInfo[name] = info.Clone()
		}
	}
	for name := range nodeNameToInfo {
		if _, ok := cache.nodes[name]; !ok {
			delete(nodeNameToInfo, name)
		}
	}
	return nil
}

func (cache *schedulerCache) List(selector labels.Selector) ([]*v1.Pod, error) {
	alwaysTrue := func(p *v1.Pod) bool { return true }
	return cache.FilteredList(alwaysTrue, selector)
}

func (cache *schedulerCache) FilteredList(podFilter PodFilter, selector labels.Selector) ([]*v1.Pod, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	// podFilter is expected to return true for most or all of the pods. We
	// can avoid expensive array growth without wasting too much memory by
	// pre-allocating capacity.
	maxSize := 0
	for _, info := range cache.nodes {
		maxSize += len(info.pods)
	}
	pods := make([]*v1.Pod, 0, maxSize)
	for _, info := range cache.nodes {
		for _, pod := range info.pods {
			if podFilter(pod) && selector.Matches(labels.Set(pod.Labels)) {
				pods = append(pods, pod)
			}
		}
	}
	return pods, nil
}

func (cache *schedulerCache) AssumePod(pod *v1.Pod) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, ok := cache.podStates[key]; ok {
		return fmt.Errorf("pod %v is in the cache, so can't be assumed", key)
	}

	cache.addPod(pod)
	ps := &podState{
		pod: pod,
	}
	cache.podStates[key] = ps
	cache.assumedPods[key] = true
	return nil
}

func (cache *schedulerCache) FinishBinding(pod *v1.Pod) error {
	return cache.finishBinding(pod, time.Now())
}

// finishBinding exists to make tests determinitistic by injecting now as an argument
func (cache *schedulerCache) finishBinding(pod *v1.Pod, now time.Time) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	glog.V(5).Infof("Finished binding for pod %v. Can be expired.", key)
	currState, ok := cache.podStates[key]
	if ok && cache.assumedPods[key] {
		dl := now.Add(cache.ttl)
		currState.bindingFinished = true
		currState.deadline = &dl
	}
	return nil
}

func (cache *schedulerCache) ForgetPod(pod *v1.Pod) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	currState, ok := cache.podStates[key]
	if ok && currState.pod.Spec.NodeName != pod.Spec.NodeName {
		return fmt.Errorf("pod %v was assumed on %v but assigned to %v", key, pod.Spec.NodeName, currState.pod.Spec.NodeName)
	}

	switch {
	// Only assumed pod can be forgotten.
	case ok && cache.assumedPods[key]:
		err := cache.removePod(pod)
		if err != nil {
			return err
		}
		delete(cache.assumedPods, key)
		delete(cache.podStates, key)
	default:
		return fmt.Errorf("pod %v wasn't assumed so cannot be forgotten", key)
	}
	return nil
}

// Assumes that lock is already acquired.
func (cache *schedulerCache) addPod(pod *v1.Pod) {
	n, ok := cache.nodes[pod.Spec.NodeName]
	if !ok {
		n = NewNodeInfo()
		cache.nodes[pod.Spec.NodeName] = n
	}
	n.AddPod(pod)
}

// this function expects valid pod, and valid, non-empty resizeRequestAnnotation json string
func getPodResizeRequirements(pod *v1.Pod) (map[string]v1.Container, *Resource, error) {
	resizeContainersMap := make(map[string]v1.Container)
	for _, c := range pod.Spec.ResizeResources.Request {
		resizeContainersMap[c.Name] = v1.Container{
							Name:      c.Name,
							Resources: c.Resources,
						}
	}
	podResource := &Resource{}
	for _, container := range pod.Spec.Containers {
		containerResourcesRequests := container.Resources.Requests.DeepCopy()
		if resizeContainer, ok := resizeContainersMap[container.Name]; ok {
			for k, v := range resizeContainer.Resources.Requests {
				containerResourcesRequests[k] = v
			}
		}
		podResource.Add(containerResourcesRequests)
	}
	return resizeContainersMap, podResource, nil
}

func (cache *schedulerCache) rollbackPodResources(oldPod, newPod *v1.Pod) {
	podKey, _ := getPodKey(oldPod)
	currPodState, _ := cache.podStates[podKey]
	cachedPod := currPodState.pod
	for i, container := range newPod.Spec.Containers {
		for _, rollbackResources := range newPod.Spec.ResizeResources.Rollback {
			if rollbackResources.Name == container.Name {
				if rollbackResources.Resources.Requests != nil {
					newPod.Spec.Containers[i].Resources.Requests = rollbackResources.Resources.Requests.DeepCopy()
					cachedPod.Spec.Containers[i].Resources.Requests = rollbackResources.Resources.Requests.DeepCopy()
				}
				if rollbackResources.Resources.Limits != nil {
					newPod.Spec.Containers[i].Resources.Limits = rollbackResources.Resources.Limits.DeepCopy()
					cachedPod.Spec.Containers[i].Resources.Limits = rollbackResources.Resources.Limits.DeepCopy()
				}
				break
			}
		}
	}
}

func (cache *schedulerCache) setupInPlaceResizeAction(oldPod, newPod *v1.Pod, resizeContainersMap map[string]v1.Container) {
	podKey, _ := getPodKey(oldPod)
	currPodState, _ := cache.podStates[podKey]
	cachedPod := currPodState.pod
	var rollbackResources []v1.ContainerResources

	for i, container := range newPod.Spec.Containers {
		resizeContainer, ok := resizeContainersMap[container.Name]
		if ok {
			// Backup current container resources for restore in case of update failure
			rollbackRes := v1.ContainerResources{
						Name:      container.Name,
						Resources: v1.ResourceRequirements{
							Requests: container.Resources.Requests.DeepCopy(),
							Limits:   container.Resources.Limits.DeepCopy(),
						},
					}
			rollbackResources = append(rollbackResources, rollbackRes)
			// Validation checks ensure pod QoS invariance, just update changed values
			if resizeContainer.Resources.Requests != nil {
				for k, v := range resizeContainer.Resources.Requests {
					newPod.Spec.Containers[i].Resources.Requests[k] = v
					cachedPod.Spec.Containers[i].Resources.Requests[k] = v
				}
			}
			if resizeContainer.Resources.Limits != nil {
				for k, v := range resizeContainer.Resources.Limits {
					newPod.Spec.Containers[i].Resources.Limits[k] = v
					cachedPod.Spec.Containers[i].Resources.Limits[k] = v
				}
			}
		}
	}
	newPod.Spec.ResizeResources.ActionVersion = newPod.ResourceVersion
	newPod.Spec.ResizeResources.Action = v1.ResizeActionUpdate
	newPod.Spec.ResizeResources.Rollback = rollbackResources
}

func (cache *schedulerCache) processPodResizeStatus(oldPod, newPod *v1.Pod) {
	// If pod resources resize status has been set, clear out action and backup annotations.
	for _, podCondition := range newPod.Status.Conditions {
		if podCondition.Type != v1.PodResourcesResizeStatus {
			continue
		}
		if podCondition.Message == newPod.Spec.ResizeResources.ActionVersion {
			// If ResizeStatus shows failure, restore previous resource values
			if podCondition.Status == v1.ConditionFalse {
				if newPod.Spec.ResizeResources.Rollback != nil {
					glog.V(4).Infof("Restoring resource values for pod %v due to a failed earlier resizing attempt", oldPod.Name)
					cache.rollbackPodResources(oldPod, newPod)
				}
			}
			newPod.Spec.ResizeResources.ActionVersion = newPod.ResourceVersion
			newPod.Spec.ResizeResources.Action = v1.ResizeActionUpdateDone
			newPod.Spec.ResizeResources.Rollback = nil
		}
		break
	}
}

func (cache *schedulerCache) checkPodDisruptionBudgetOk(pod *v1.Pod) (bool, error) {
	pdbs, err := cache.listPDBs(labels.Everything())
	if err != nil {
		errMsg := fmt.Sprintf("Failure getting PDBs for pod %s. Error: %v", pod.Name, err)
		glog.Error(errMsg)
		return false, errors.New(errMsg)
	}
	for _, pdb := range pdbs {
		if selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector); err == nil {
			if selector.Empty() || !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			if pdb.Status.PodDisruptionsAllowed <= 0 {
				glog.V(4).Infof("Rescheduling pod %s violates disruption budget %s.", pod.Name, pdb.Name)
				return false, nil
			}
		} else {
			errMsg := fmt.Sprintf("Failure transforming LabelSelector for pdb %s. Error: %v", pdb.Name, err)
			glog.Error(errMsg)
			return false, errors.New(errMsg)
		}
	}
	return true, nil
}

func (cache *schedulerCache) processPodResourcesScaling(oldPod, newPod *v1.Pod) error {
	node, ok := cache.nodes[newPod.Spec.NodeName]
	if !ok {
		errMsg := fmt.Sprintf("Node %s not found for pod %s", newPod.Spec.NodeName, newPod.Name)
		glog.Error(errMsg)
		return errors.New(errMsg)
	}

	// resource resize policy defaults to InPlacePreferred
	resizeResourcesPolicy := v1.ResizePolicyInPlacePreferred
	if newPod.Spec.ResizeResourcesPolicy != "" {
		resizeResourcesPolicy = newPod.Spec.ResizeResourcesPolicy
	}

	cache.processPodResizeStatus(oldPod, newPod)

	if len(newPod.Spec.ResizeResources.Request) != 0 {
		if resizeResourcesPolicy == v1.ResizePolicyRestart {
			newPod.Spec.ResizeResources.Request = nil
			newPod.Spec.ResizeResources.ActionVersion = newPod.ResourceVersion
			newPod.Spec.ResizeResources.Action = v1.ResizeActionReschedule
			glog.V(4).Infof("Rescheduling pod %s due to ResizePolicyRestart.", newPod.Name)
			return nil
		}

		if resizeContainersMap, podResource, err := getPodResizeRequirements(newPod); err == nil {
			newPod.Spec.ResizeResources.Request = nil
			allocatable := node.AllocatableResource()
			nodeMilliCPU := node.RequestedResource().MilliCPU
			nodeMemory := node.RequestedResource().Memory
			if (allocatable.MilliCPU > (podResource.MilliCPU + nodeMilliCPU)) &&
				(allocatable.Memory > (podResource.Memory + nodeMemory)) {
				// InPlace resizing is possible
				cache.setupInPlaceResizeAction(oldPod, newPod, resizeContainersMap)
				return nil
			} else {
				// InPlace resizing is not possible, restart if allowed by policy
				newPod.Spec.ResizeResources.ActionVersion = newPod.ResourceVersion
				if resizeResourcesPolicy == v1.ResizePolicyInPlaceOnly {
					newPod.Spec.ResizeResources.Action = v1.ResizeActionNonePerPolicy
					glog.V(4).Infof("In-place resizing of pod %s on node %s rejected by policy (%s). Allocatable CPU: %d, Memory: %d. Requested: CPU: %d, Memory %d.",
						newPod.Name, newPod.Spec.NodeName, resizeResourcesPolicy, allocatable.MilliCPU, allocatable.Memory, podResource.MilliCPU, podResource.Memory)
					return nil
				}
				// Check for pod disruption budget violations
				if len(newPod.Labels) > 0 {
					ok, err := cache.checkPodDisruptionBudgetOk(newPod)
					if err != nil {
						return err
					}
					if !ok {
						// Skip rescheduling at this time as it violates PDB. Let the controller retries handle it.
						newPod.Spec.ResizeResources.Action = v1.ResizeActionNonePerPDBViolation
						return nil
					}
					glog.V(4).Infof("Rescheduling pod %s as it is within disruption budget.", newPod.Name)
				}
				newPod.Spec.ResizeResources.Action = v1.ResizeActionReschedule
			}
		} else {
			glog.Errorf("Pod %s getPodResizeRequirements failed. Error: %v", newPod.Name, err)
			return err
		}
	}
	return nil
}

// Assumes that lock is already acquired.
func (cache *schedulerCache) updatePod(oldPod, newPod *v1.Pod) error {
	var err error
	if err := cache.removePod(oldPod); err != nil {
		return err
	}
	// Resize request is valid for running pods
	if utilfeature.DefaultFeatureGate.Enabled(features.VerticalScaling) &&
		oldPod.Status.Phase == v1.PodRunning && newPod.Status.Phase == v1.PodRunning &&
		newPod.DeletionTimestamp == nil && newPod.Spec.ResizeResources != nil {
		err = cache.processPodResourcesScaling(oldPod, newPod)
	}
	cache.addPod(newPod)
	return err
}

// Assumes that lock is already acquired.
func (cache *schedulerCache) removePod(pod *v1.Pod) error {
	n := cache.nodes[pod.Spec.NodeName]
	if err := n.RemovePod(pod); err != nil {
		return err
	}
	if len(n.pods) == 0 && n.node == nil {
		delete(cache.nodes, pod.Spec.NodeName)
	}
	return nil
}

func (cache *schedulerCache) AddPod(pod *v1.Pod) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	currState, ok := cache.podStates[key]
	switch {
	case ok && cache.assumedPods[key]:
		if currState.pod.Spec.NodeName != pod.Spec.NodeName {
			// The pod was added to a different node than it was assumed to.
			glog.Warningf("Pod %v was assumed to be on %v but got added to %v", key, pod.Spec.NodeName, currState.pod.Spec.NodeName)
			// Clean this up.
			cache.removePod(currState.pod)
			cache.addPod(pod)
		}
		delete(cache.assumedPods, key)
		cache.podStates[key].deadline = nil
		cache.podStates[key].pod = pod
	case !ok:
		// Pod was expired. We should add it back.
		cache.addPod(pod)
		ps := &podState{
			pod: pod,
		}
		cache.podStates[key] = ps
	default:
		return fmt.Errorf("pod %v was already in added state", key)
	}
	return nil
}

func (cache *schedulerCache) UpdatePod(oldPod, newPod *v1.Pod) error {
	key, err := getPodKey(oldPod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	currState, ok := cache.podStates[key]
	switch {
	// An assumed pod won't have Update/Remove event. It needs to have Add event
	// before Update event, in which case the state would change from Assumed to Added.
	case ok && !cache.assumedPods[key]:
		if currState.pod.Spec.NodeName != newPod.Spec.NodeName {
			glog.Errorf("Pod %v updated on a different node than previously added to.", key)
			glog.Fatalf("Schedulercache is corrupted and can badly affect scheduling decisions")
		}
		if err := cache.updatePod(oldPod, newPod); err != nil {
			return err
		}
	default:
		return fmt.Errorf("pod %v is not added to scheduler cache, so cannot be updated", key)
	}
	return nil
}

func (cache *schedulerCache) RemovePod(pod *v1.Pod) error {
	key, err := getPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	currState, ok := cache.podStates[key]
	switch {
	// An assumed pod won't have Delete/Remove event. It needs to have Add event
	// before Remove event, in which case the state would change from Assumed to Added.
	case ok && !cache.assumedPods[key]:
		if currState.pod.Spec.NodeName != pod.Spec.NodeName {
			glog.Errorf("Pod %v was assumed to be on %v but got added to %v", key, pod.Spec.NodeName, currState.pod.Spec.NodeName)
			glog.Fatalf("Schedulercache is corrupted and can badly affect scheduling decisions")
		}
		err := cache.removePod(currState.pod)
		if err != nil {
			return err
		}
		delete(cache.podStates, key)
	default:
		return fmt.Errorf("pod %v is not found in scheduler cache, so cannot be removed from it", key)
	}
	return nil
}

func (cache *schedulerCache) IsAssumedPod(pod *v1.Pod) (bool, error) {
	key, err := getPodKey(pod)
	if err != nil {
		return false, err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	b, found := cache.assumedPods[key]
	if !found {
		return false, nil
	}
	return b, nil
}

func (cache *schedulerCache) GetPod(pod *v1.Pod) (*v1.Pod, error) {
	key, err := getPodKey(pod)
	if err != nil {
		return nil, err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	podState, ok := cache.podStates[key]
	if !ok {
		return nil, fmt.Errorf("pod %v does not exist in scheduler cache", key)
	}

	return podState.pod, nil
}

func (cache *schedulerCache) AddNode(node *v1.Node) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	n, ok := cache.nodes[node.Name]
	if !ok {
		n = NewNodeInfo()
		cache.nodes[node.Name] = n
	}
	return n.SetNode(node)
}

func (cache *schedulerCache) UpdateNode(oldNode, newNode *v1.Node) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	n, ok := cache.nodes[newNode.Name]
	if !ok {
		n = NewNodeInfo()
		cache.nodes[newNode.Name] = n
	}
	return n.SetNode(newNode)
}

func (cache *schedulerCache) RemoveNode(node *v1.Node) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	n := cache.nodes[node.Name]
	if err := n.RemoveNode(node); err != nil {
		return err
	}
	// We remove NodeInfo for this node only if there aren't any pods on this node.
	// We can't do it unconditionally, because notifications about pods are delivered
	// in a different watch, and thus can potentially be observed later, even though
	// they happened before node removal.
	if len(n.pods) == 0 && n.node == nil {
		delete(cache.nodes, node.Name)
	}
	return nil
}

func (cache *schedulerCache) AddPDB(pdb *policy.PodDisruptionBudget) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	// Unconditionally update cache.
	cache.pdbs[string(pdb.UID)] = pdb
	return nil
}

func (cache *schedulerCache) UpdatePDB(oldPDB, newPDB *policy.PodDisruptionBudget) error {
	return cache.AddPDB(newPDB)
}

func (cache *schedulerCache) RemovePDB(pdb *policy.PodDisruptionBudget) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	delete(cache.pdbs, string(pdb.UID))
	return nil
}

// Assumes that lock is already acquired.
func (cache *schedulerCache) listPDBs(selector labels.Selector) ([]*policy.PodDisruptionBudget, error) {
	var pdbs []*policy.PodDisruptionBudget
	for _, pdb := range cache.pdbs {
		if selector.Matches(labels.Set(pdb.Labels)) {
			pdbs = append(pdbs, pdb)
		}
	}
	return pdbs, nil
}

func (cache *schedulerCache) ListPDBs(selector labels.Selector) ([]*policy.PodDisruptionBudget, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.listPDBs(selector)
}

func (cache *schedulerCache) IsUpToDate(n *NodeInfo) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	node, ok := cache.nodes[n.Node().Name]
	return ok && n.generation == node.generation
}

func (cache *schedulerCache) run() {
	go wait.Until(cache.cleanupExpiredAssumedPods, cache.period, cache.stop)
}

func (cache *schedulerCache) cleanupExpiredAssumedPods() {
	cache.cleanupAssumedPods(time.Now())
}

// cleanupAssumedPods exists for making test deterministic by taking time as input argument.
func (cache *schedulerCache) cleanupAssumedPods(now time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	// The size of assumedPods should be small
	for key := range cache.assumedPods {
		ps, ok := cache.podStates[key]
		if !ok {
			panic("Key found in assumed set but not in podStates. Potentially a logical error.")
		}
		if !ps.bindingFinished {
			glog.V(3).Infof("Couldn't expire cache for pod %v/%v. Binding is still in progress.",
				ps.pod.Namespace, ps.pod.Name)
			continue
		}
		if now.After(*ps.deadline) {
			glog.Warningf("Pod %s/%s expired", ps.pod.Namespace, ps.pod.Name)
			if err := cache.expirePod(key, ps); err != nil {
				glog.Errorf("ExpirePod failed for %s: %v", key, err)
			}
		}
	}
}

func (cache *schedulerCache) expirePod(key string, ps *podState) error {
	if err := cache.removePod(ps.pod); err != nil {
		return err
	}
	delete(cache.assumedPods, key)
	delete(cache.podStates, key)
	return nil
}
