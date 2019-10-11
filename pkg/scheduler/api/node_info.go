/*
Copyright 2017 The Kubernetes Authors.

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

package api

import (
	"fmt"

	"github.com/golang/glog"

	v1 "k8s.io/api/core/v1"
)

// NodeInfo is node level aggregated information.
type NodeInfo struct {
	Name string
	Node *v1.Node

	// The state of node
	State NodeState

	// The releasing resource on that node
	Releasing *Resource
	// The idle resource on that node
	Idle *Resource
	// The used resource on that node, including running and terminating
	// pods
	Used *Resource
	// The used resource on the node occupied by backfill tasks
	Backfilled *Resource

	Allocatable *Resource
	Capability  *Resource

	Tasks map[TaskID]*TaskInfo
}

// NodeState defines the current state of node.
type NodeState struct {
	Phase  NodePhase
	Reason string
}

// NewNodeInfo is used to create new nodeInfo object
func NewNodeInfo(node *v1.Node) *NodeInfo {
	var ni *NodeInfo

	if node == nil {
		ni = &NodeInfo{
			Releasing:  EmptyResource(),
			Idle:       EmptyResource(),
			Used:       EmptyResource(),
			Backfilled: EmptyResource(),

			Allocatable: EmptyResource(),
			Capability:  EmptyResource(),

			Tasks: make(map[TaskID]*TaskInfo),
		}
	} else {
		ni = &NodeInfo{
			Name: node.Name,
			Node: node,

			Releasing:  EmptyResource(),
			Idle:       NewResource(node.Status.Allocatable),
			Used:       EmptyResource(),
			Backfilled: EmptyResource(),

			Allocatable: NewResource(node.Status.Allocatable),
			Capability:  NewResource(node.Status.Capacity),

			Tasks: make(map[TaskID]*TaskInfo),
		}
	}

	ni.setNodeState(node)

	return ni
}

// Clone used to clone nodeInfo Object
func (ni *NodeInfo) Clone() *NodeInfo {
	res := NewNodeInfo(ni.Node)
	glog.V(4).Infof("new node <%v>: capability %v,  allocatable %v, idle %v, used %v, backfilled %v, releasing %v", ni.Name, ni.Capability.MilliCPU,
		ni.Allocatable.MilliCPU,
		ni.Idle.MilliCPU,
		ni.Used.MilliCPU,
		ni.Backfilled.MilliCPU,
		ni.Releasing.MilliCPU)

	for _, task := range ni.Tasks {
		glog.V(4).Infof("Adding task <%v/%v> to node <%v> with resource request %v", task.Namespace, task.Name, ni.Name, task.Resreq)
		res.AddTask(task)
	}

	return res
}

// Ready returns whether node is ready for scheduling
func (ni *NodeInfo) Ready() bool {
	return ni.State.Phase == Ready
}

func (ni *NodeInfo) setNodeState(node *v1.Node) {
	// If node is nil, the node is un-initialized in cache
	if node == nil {
		ni.State = NodeState{
			Phase:  NotReady,
			Reason: "UnInitialized",
		}
		return
	}

	// set NodeState according to resources
	if !ni.Used.LessEqual(NewResource(node.Status.Allocatable)) {
		ni.State = NodeState{
			Phase:  NotReady,
			Reason: "OutOfSync",
		}
		return
	}

	// Node is ready (ignore node conditions because of taint/toleration)
	ni.State = NodeState{
		Phase:  Ready,
		Reason: "",
	}
}

// SetNode sets kubernetes node object to nodeInfo object
func (ni *NodeInfo) SetNode(node *v1.Node) {
	ni.setNodeState(node)

	if !ni.Ready() {
		glog.Warningf("Failed to set node info, phase: %s, reason: %s",
			ni.State.Phase, ni.State.Reason)
		return
	}

	ni.Name = node.Name
	ni.Node = node

	ni.Allocatable = NewResource(node.Status.Allocatable)
	ni.Capability = NewResource(node.Status.Capacity)
	ni.Idle = NewResource(node.Status.Allocatable)
	ni.Used = EmptyResource()

	for _, task := range ni.Tasks {
		if task.Status == Releasing {
			ni.Releasing.Add(task.Resreq)
		}

		ni.Idle.Sub(task.Resreq)
		ni.Used.Add(task.Resreq)
	}
}

func (ni *NodeInfo) allocateIdleResource(ti *TaskInfo) error {
	if ti.Resreq.LessEqual(ni.Idle) {
		ni.Idle.Sub(ti.Resreq)
		return nil
	}
	ni.State = NodeState{
		Phase:  NotReady,
		Reason: "OutOfSync",
	}
	return fmt.Errorf("Selected node NotReady")
}

// AddTask is used to add a task in nodeInfo object
func (ni *NodeInfo) AddTask(task *TaskInfo) error {
	key := PodKey(task.Pod)
	if _, found := ni.Tasks[key]; found {
		return fmt.Errorf("task <%v/%v> already on node <%v>",
			task.Namespace, task.Name, ni.Name)
	}

	// Node will hold a copy of task to make sure the status
	// change will not impact resource in node.
	ti := task.Clone()

	if ni.Node != nil {
		if task.Condition.IsBackfill {
			ni.Backfilled.Add(task.Resreq)
		}

		switch ti.Status {
		case Releasing:
			if err := ni.allocateIdleResource(ti); err != nil {
				return err
			}
			ni.Releasing.Add(ti.Resreq)
		case Pipelined:
			ni.Releasing.Sub(ti.Resreq)
		default:
			if err := ni.allocateIdleResource(ti); err != nil {
				return err
			}
		}

		// removing pod resources not maintained by kube-batch from node.ALlocatable
		if ti.Job == "" && ti.Pod.Status.Phase == v1.PodRunning {
			glog.Infof("adjusted Allocatable on node %s by %v", ni.Name, ti.Resreq)
			ni.Allocatable.Sub(ti.Resreq)
		}

		ni.Used.Add(ti.Resreq)
	}

	ni.Tasks[key] = ti

	return nil
}

// RemoveTask used to remove a task from nodeInfo object
func (ni *NodeInfo) RemoveTask(ti *TaskInfo) error {
	key := PodKey(ti.Pod)

	task, found := ni.Tasks[key]
	if !found {
		return fmt.Errorf("failed to find task <%v/%v> on host <%v>",
			ti.Namespace, ti.Name, ni.Name)
	}

	if ni.Node != nil {
		if task.Condition.IsBackfill {
			ni.Backfilled.Sub(task.Resreq)
		}

		switch task.Status {
		case Releasing:
			ni.Releasing.Sub(task.Resreq)
			ni.Idle.Add(task.Resreq)
		case Pipelined:
			ni.Releasing.Add(task.Resreq)
		default:
			ni.Idle.Add(task.Resreq)
		}

		ni.Used.Sub(task.Resreq)
	}

	delete(ni.Tasks, key)

	return nil
}

// UpdateTask is used to update a task in nodeInfo object
func (ni *NodeInfo) UpdateTask(ti *TaskInfo) error {
	if err := ni.RemoveTask(ti); err != nil {
		return err
	}

	return ni.AddTask(ti)
}

// String returns nodeInfo details in string format
func (ni NodeInfo) String() string {
	tasks := ""

	i := 0
	for _, task := range ni.Tasks {
		tasks = tasks + fmt.Sprintf("\n\t %d: %v", i, task)
		i++
	}

	return fmt.Sprintf("Node (%s): idle <%v>, used <%v>, releasing <%v>, backfill <%v>, taints <%v>%s",
		ni.Name, ni.Idle, ni.Used, ni.Releasing, ni.Backfilled, ni.Node.Spec.Taints, tasks)

}

// Pods returns all pods running in that node
func (ni *NodeInfo) Pods() (pods []*v1.Pod) {
	for _, t := range ni.Tasks {
		pods = append(pods, t.Pod)
	}

	return
}

// GetAccessibleResource get accessible resource which equals idle + backfilled
func (ni *NodeInfo) GetAccessibleResource() *Resource {
	idle := ni.Idle.Clone()
	accessible := idle.Add(ni.Backfilled)
	glog.V(3).Infof("Accessible resources on Node <%v>: %v. Idle: %v. Used: %v. Backfilled: %v", ni.Name, accessible, ni.Idle, ni.Used, ni.Backfilled)
	return accessible
}
