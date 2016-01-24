/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	client "k8s.io/kubernetes/pkg/client/unversioned"
)

type Policy struct {
	unversioned.TypeMeta `json:",inline"`
	// Holds the information to configure the fit predicate functions
	Predicates []PredicatePolicy `json:"predicates"`
	// Holds the information to configure the priority functions
	Priorities []PriorityPolicy `json:"priorities"`
	// Holds the information to communicate with the extender(s)
	ExtenderConfigs []ExtenderConfig `json:"extenders"`
}

type PredicatePolicy struct {
	// Identifier of the predicate policy
	// For a custom predicate, the name can be user-defined
	// For the Kubernetes provided predicates, the name is the identifier of the pre-defined predicate
	Name string `json:"name"`
	// Holds the parameters to configure the given predicate
	Argument *PredicateArgument `json:"argument"`
}

type PriorityPolicy struct {
	// Identifier of the priority policy
	// For a custom priority, the name can be user-defined
	// For the Kubernetes provided priority functions, the name is the identifier of the pre-defined priority function
	Name string `json:"name"`
	// The numeric multiplier for the node scores that the priority function generates
	// The weight should be a positive integer
	Weight int `json:"weight"`
	// Holds the parameters to configure the given priority function
	Argument *PriorityArgument `json:"argument"`
}

// Represents the arguments that the different types of predicates take
// Only one of its members may be specified
type PredicateArgument struct {
	// The predicate that provides affinity for pods belonging to a service
	// It uses a label to identify nodes that belong to the same "group"
	ServiceAffinity *ServiceAffinity `json:"serviceAffinity"`
	// The predicate that checks whether a particular node has a certain label
	// defined or not, regardless of value
	LabelsPresence *LabelsPresence `json:"labelsPresence"`
}

// Represents the arguments that the different types of priorities take.
// Only one of its members may be specified
type PriorityArgument struct {
	// The priority function that ensures a good spread (anti-affinity) for pods belonging to a service
	// It uses a label to identify nodes that belong to the same "group"
	ServiceAntiAffinity *ServiceAntiAffinity `json:"serviceAntiAffinity"`
	// The priority function that checks whether a particular node has a certain label
	// defined or not, regardless of value
	LabelPreference *LabelPreference `json:"labelPreference"`
}

// Holds the parameters that are used to configure the corresponding predicate
type ServiceAffinity struct {
	// The list of labels that identify node "groups"
	// All of the labels should match for the node to be considered a fit for hosting the pod
	Labels []string `json:"labels"`
}

// Holds the parameters that are used to configure the corresponding predicate
type LabelsPresence struct {
	// The list of labels that identify node "groups"
	// All of the labels should be either present (or absent) for the node to be considered a fit for hosting the pod
	Labels []string `json:"labels"`
	// The boolean flag that indicates whether the labels should be present or absent from the node
	Presence bool `json:"presence"`
}

// Holds the parameters that are used to configure the corresponding priority function
type ServiceAntiAffinity struct {
	// Used to identify node "groups"
	Label string `json:"label"`
}

// Holds the parameters that are used to configure the corresponding priority function
type LabelPreference struct {
	// Used to identify node "groups"
	Label string `json:"label"`
	// This is a boolean flag
	// If true, higher priority is given to nodes that have the label
	// If false, higher priority is given to nodes that do not have the label
	Presence bool `json:"presence"`
}

// Holds the parameters used to communicate with the extender. If a verb is unspecified/empty,
// it is assumed that the extender chose not to provide that extension.
type ExtenderConfig struct {
	// URLPrefix at which the extender is available
	URLPrefix string `json:"urlPrefix"`
	// Verb for the filter call, empty if not supported. This verb is appended to the URLPrefix when issuing the filter call to extender.
	FilterVerb string `json:"filterVerb,omitempty"`
	// Verb for the prioritize call, empty if not supported. This verb is appended to the URLPrefix when issuing the prioritize call to extender.
	PrioritizeVerb string `json:"prioritizeVerb,omitempty"`
	// The numeric multiplier for the node scores that the prioritize call generates.
	// The weight should be a positive integer
	Weight int `json:"weight,omitempty"`
	// EnableHttps specifies whether https should be used to communicate with the extender
	EnableHttps bool `json:"enableHttps,omitempty"`
	// TLSConfig specifies the transport layer security config
	TLSConfig *client.TLSClientConfig `json:"tlsConfig,omitempty"`
	// HTTPTimeout specifies the timeout duration for a call to the extender. Filter timeout fails the scheduling of the pod. Prioritize
	// timeout is ignored, k8s/other extenders priorities are used to select the node.
	HTTPTimeout time.Duration `json:"httpTimeout,omitempty"`
}

// ExtenderArgs represents the arguments needed by the extender to filter/prioritize
// nodes for a pod.
type ExtenderArgs struct {
	// Pod being scheduled
	Pod api.Pod `json:"pod"`
	// List of candidate nodes where the pod can be scheduled
	Nodes api.NodeList `json:"nodes"`
}

// ExtenderFilterResult represents the results of a filter call to an extender
type ExtenderFilterResult struct {
	// Filtered set of nodes where the pod can be scheduled
	Nodes api.NodeList `json:"nodes,omitempty"`
	// Error message indicating failure
	Error string `json:"error,omitempty"`
}

// HostPriority represents the priority of scheduling to a particular host, higher priority is better.
type HostPriority struct {
	// Name of the host
	Host string `json:"host"`
	// Score associated with the host
	Score int `json:"score"`
}

type HostPriorityList []HostPriority

func (h HostPriorityList) Len() int {
	return len(h)
}

func (h HostPriorityList) Less(i, j int) bool {
	if h[i].Score == h[j].Score {
		return h[i].Host < h[j].Host
	}
	return h[i].Score < h[j].Score
}

func (h HostPriorityList) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

// A node selector represents the union of the results of one or more label queries
// over a set of nodes; that is, it represents the OR of the selectors represented
// by the nodeSelectorTerms.
type NodeSelector struct {
	// nodeSelectorTerms is a list of node selector terms. The terms are ORed.
	NodeSelectorTerms []NodeSelectorTerm `json:"nodeSelectorTerms,omitempty"`
}

// A null or empty node selector term matches no objects.
type NodeSelectorTerm struct {
	// matchExpressions is a list of node selector requirements. The requirements are ANDed.
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty"`
}

// A node selector requirement is a selector that contains values, a key, and an operator
// that relates the key and values.
type NodeSelectorRequirement struct {
	// key is the label key that the selector applies to.
	Key string `json:"key" patchStrategy:"merge" patchMergeKey:"key"`
	// operator represents a key's relationship to a set of values.
	// Valid operators are In, NotIn, Exists, DoesNotExist. Gt, and Lt.
	Operator NodeSelectorOperator `json:"operator"`
	// values is an array of string values. If the operator is In or NotIn,
	// the values array must be non-empty. If the operator is Exists or DoesNotExist,
	// the values array must be empty. If the operator is Gt or Lt, the values
	// array must have a single element, which will be interpreted as an integer.
	// This array is replaced during a strategic merge patch.
	Values []string `json:"values,omitempty"`
}

// A node selector operator is the set of operators that can be used in
// a node selector requirement.
type NodeSelectorOperator string

const (
	NodeSelectorOpIn           NodeSelectorOperator = "In"
	NodeSelectorOpNotIn        NodeSelectorOperator = "NotIn"
	NodeSelectorOpExists       NodeSelectorOperator = "Exists"
	NodeSelectorOpDoesNotExist NodeSelectorOperator = "DoesNotExist"
	NodeSelectorOpGt           NodeSelectorOperator = "Gt"
	NodeSelectorOpLt           NodeSelectorOperator = "Lt"
)

// An Affinity is a group of affinity scheduling requirements,
// including node affinity and inter pod affinity.
type Affinity struct {
	NodeAffinity *NodeAffinity `json:"nodeAffinity,omitempty"`
}

// An NodeAffinity is a group of node affinity scheduling requirements.
type NodeAffinity struct {
	// If the affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to an update), the system
	// will try to eventually evict the pod from its node.
	RequiredDuringSchedulingRequiredDuringExecution *NodeSelector `json:"requiredDuringSchedulingRequiredDuringExecution,omitempty"`
	// If the affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to an update), the system
	// may or may not try to eventually evict the pod from its node.
	RequiredDuringSchedulingIgnoredDuringExecution *NodeSelector `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	// The scheduler will prefer to schedule pods to nodes that satisfy
	// the affinity expressions specified by this field, but it may choose
	// a node that violates one or more of the expressions. The node that is
	// most preferred is the one with the greatest sum of weights, i.e.
	// for each node that meets all of the scheduling requirements (resource
	// request, RequiredDuringScheduling affinity expressions, etc.),
	// compute a sum by iterating through the elements of this field and adding
	// "weight" to the sum if the node matches the corresponding MatchExpressions; the
	// node(s) with the highest sum are the most preferred.
	PreferredDuringSchedulingIgnoredDuringExecution []PreferredSchedulingTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// An empty preferred scheduling term matches all objects with implicit weight 0
// (i.e. it's a no-op). A null preferred scheduling term matches no objects.
type PreferredSchedulingTerm struct {
	// weight is in the range 1-100
	Weight int `json:"weight"`
	// matchExpressions is a list of node selector requirements. The requirements are ANDed.
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty"`
}

// AffinityAnnotationKey represents the key of affinity data(json serialized)
// in the Annotations of a Pod
const AffinityAnnotationKey string = "scheduler.alpha.kubernetes.io/affinity"
