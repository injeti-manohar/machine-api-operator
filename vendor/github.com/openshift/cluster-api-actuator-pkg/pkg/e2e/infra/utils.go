package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/glog"
	e2e "github.com/openshift/cluster-api-actuator-pkg/pkg/e2e/framework"
	mapiv1beta1 "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	controllernode "github.com/openshift/cluster-api/pkg/controller/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

const (
	nodeWorkerRoleLabel = "node-role.kubernetes.io/worker"
	machineRoleLabel    = "machine.openshift.io/cluster-api-machine-role"
	machineAPIGroup     = "machine.openshift.io"
)

func isOneMachinePerNode(client runtimeclient.Client) bool {
	listOptions := runtimeclient.ListOptions{
		Namespace: e2e.TestContext.MachineApiNamespace,
	}
	machineList := mapiv1beta1.MachineList{}
	nodeList := corev1.NodeList{}

	if err := wait.PollImmediate(5*time.Second, e2e.WaitShort, func() (bool, error) {
		if err := client.List(context.TODO(), &listOptions, &machineList); err != nil {
			glog.Errorf("Error querying api for machineList object: %v, retrying...", err)
			return false, nil
		}
		if err := client.List(context.TODO(), &listOptions, &nodeList); err != nil {
			glog.Errorf("Error querying api for nodeList object: %v, retrying...", err)
			return false, nil
		}

		glog.Infof("Expecting the same number of machines and nodes, have %d nodes and %d machines", len(nodeList.Items), len(machineList.Items))
		if len(machineList.Items) != len(nodeList.Items) {
			return false, nil
		}

		nodeNameToMachineAnnotation := make(map[string]string)
		for _, node := range nodeList.Items {
			if _, ok := node.Annotations[controllernode.MachineAnnotationKey]; !ok {
				glog.Errorf("Node %q does not have a MachineAnnotationKey %q, retrying...", node.Name, controllernode.MachineAnnotationKey)
				return false, nil
			}
			nodeNameToMachineAnnotation[node.Name] = node.Annotations[controllernode.MachineAnnotationKey]
		}
		for _, machine := range machineList.Items {
			if machine.Status.NodeRef == nil {
				glog.Errorf("Machine %q has no NodeRef, retrying...", machine.Name)
				return false, nil
			}
			nodeName := machine.Status.NodeRef.Name
			if nodeNameToMachineAnnotation[nodeName] != fmt.Sprintf("%s/%s", e2e.TestContext.MachineApiNamespace, machine.Name) {
				glog.Errorf("Node name %q does not match expected machine name %q, retrying...", nodeName, machine.Name)
				return false, nil
			}
			glog.Infof("Machine %q is linked to node %q", machine.Name, nodeName)
		}
		return true, nil
	}); err != nil {
		glog.Errorf("Error checking isOneMachinePerNode: %v", err)
		return false
	}
	return true
}

// getClusterSize returns the number of nodes of the cluster
func getClusterSize(client runtimeclient.Client) (int, error) {
	nodes, err := e2e.GetNodes(client)
	if err != nil {
		return 0, fmt.Errorf("error getting nodes: %v", err)
	}

	glog.Infof("Cluster size is %d nodes", len(nodes))
	return len(nodes), nil
}

// machineSetsSnapShotLogs logs the state of all the machineSets in the cluster
func machineSetsSnapShotLogs(client runtimeclient.Client) error {
	machineSets, err := e2e.GetMachineSets(context.TODO(), client)
	if err != nil {
		return fmt.Errorf("error getting machines: %v", err)
	}

	for _, machineset := range machineSets {
		glog.Infof("MachineSet %q replicas %d. Ready: %d, available %d",
			machineset.Name,
			pointer.Int32PtrDerefOr(machineset.Spec.Replicas, e2e.DefaultMachineSetReplicas),
			machineset.Status.ReadyReplicas,
			machineset.Status.AvailableReplicas)
	}
	return nil
}

// getMachinesFromMachineSet returns an array of machines owned by a given machineSet
func getMachinesFromMachineSet(client runtimeclient.Client, machineSet mapiv1beta1.MachineSet) ([]mapiv1beta1.Machine, error) {
	machines, err := e2e.GetMachines(context.TODO(), client)
	if err != nil {
		return nil, fmt.Errorf("error getting machines: %v", err)
	}
	var machinesForSet []mapiv1beta1.Machine
	for key := range machines {
		if metav1.IsControlledBy(&machines[key], &machineSet) {
			machinesForSet = append(machinesForSet, machines[key])
		}
	}
	return machinesForSet, nil
}

// getMachineFromNode returns the machine referenced by the "controllernode.MachineAnnotationKey" annotation in the given node
func getMachineFromNode(client runtimeclient.Client, node *corev1.Node) (*mapiv1beta1.Machine, error) {
	machineNamespaceKey, ok := node.Annotations[controllernode.MachineAnnotationKey]
	if !ok {
		return nil, fmt.Errorf("node %q does not have a MachineAnnotationKey %q", node.Name, controllernode.MachineAnnotationKey)
	}
	namespace, machineName, err := cache.SplitMetaNamespaceKey(machineNamespaceKey)
	if err != nil {
		return nil, fmt.Errorf("machine annotation format is incorrect %v: %v", machineNamespaceKey, err)
	}

	if namespace != e2e.TestContext.MachineApiNamespace {
		return nil, fmt.Errorf("Machine %q is forbidden to live outside of default %v namespace", machineNamespaceKey, e2e.TestContext.MachineApiNamespace)
	}

	machine, err := e2e.GetMachine(context.TODO(), client, machineName)
	if err != nil {
		return nil, fmt.Errorf("error querying api for machine object: %v", err)
	}

	return machine, nil
}

// deleteMachine deletes a specific machine and returns an error otherwise
func deleteMachine(client runtimeclient.Client, machine *mapiv1beta1.Machine) error {
	return wait.PollImmediate(1*time.Second, time.Minute, func() (bool, error) {
		if err := client.Delete(context.TODO(), machine); err != nil {
			glog.Errorf("Error querying api for machine object %q: %v, retrying...", machine.Name, err)
			return false, err
		}
		return true, nil
	})
}

// getNodesFromMachineSet returns an array of nodes backed by machines owned by a given machineSet
func getNodesFromMachineSet(client runtimeclient.Client, machineSet mapiv1beta1.MachineSet) ([]*corev1.Node, error) {
	machines, err := getMachinesFromMachineSet(client, machineSet)
	if err != nil {
		return nil, fmt.Errorf("error calling getMachinesFromMachineSet %v", err)
	}

	var nodes []*corev1.Node
	for key := range machines {
		node, err := getNodeFromMachine(client, &machines[key])
		if err != nil {
			return nil, fmt.Errorf("error getting node from machine %q: %v", machines[key].Name, err)
		}
		nodes = append(nodes, node)
	}
	glog.Infof("MachineSet %q have %d nodes", machineSet.Name, len(nodes))
	return nodes, nil
}

// getNodeFromMachine returns the node object referenced by machine.Status.NodeRef
func getNodeFromMachine(client runtimeclient.Client, machine *mapiv1beta1.Machine) (*corev1.Node, error) {
	var node corev1.Node
	if machine.Status.NodeRef == nil {
		glog.Errorf("Machine %q has no NodeRef", machine.Name)
		return nil, fmt.Errorf("machine %q has no NodeRef", machine.Name)
	}
	key := runtimeclient.ObjectKey{Namespace: machine.Status.NodeRef.Namespace, Name: machine.Status.NodeRef.Name}
	if err := client.Get(context.Background(), key, &node); err != nil {
		return nil, fmt.Errorf("error getting node %q: %v", node.Name, err)
	}

	glog.Infof("Machine %q is backing node %q", machine.Name, node.Name)
	return &node, nil
}

// getWorkerNode returns a node with the nodeWorkerRoleLabel label
func getWorkerNode(client runtimeclient.Client) (*corev1.Node, error) {
	nodeList := corev1.NodeList{}
	listOptions := runtimeclient.ListOptions{}
	listOptions.MatchingLabels(map[string]string{nodeWorkerRoleLabel: ""})
	if err := wait.PollImmediate(1*time.Second, time.Minute, func() (bool, error) {
		if err := client.List(context.TODO(), &listOptions, &nodeList); err != nil {
			glog.Errorf("Error querying api for nodeList object: %v, retrying...", err)
			return false, nil
		}
		if len(nodeList.Items) < 1 {
			glog.Errorf("No nodes were found with label %q", nodeWorkerRoleLabel)
			return false, nil
		}
		return true, nil
	}); err != nil {
		glog.Errorf("Error calling getWorkerMachine: %v", err)
		return nil, err
	}
	return &nodeList.Items[0], nil
}

// nodesAreReady returns true if an array of nodes are all ready
func nodesAreReady(nodes []*corev1.Node) bool {
	// All nodes needs to be ready
	for key := range nodes {
		if !e2e.IsNodeReady(nodes[key]) {
			glog.Errorf("Node %q is not ready. Conditions are: %v", nodes[key].Name, nodes[key].Status.Conditions)
			return false
		}
		glog.Infof("Node %q is ready. Conditions are: %v", nodes[key].Name, nodes[key].Status.Conditions)
	}
	return true
}

// scaleMachineSet scales a machineSet with a given name to the given number of replicas
func scaleMachineSet(name string, replicas int) error {
	scaleClient, err := getScaleClient()
	if err != nil {
		return fmt.Errorf("error calling getScaleClient %v", err)
	}

	scale, err := scaleClient.Scales(e2e.TestContext.MachineApiNamespace).Get(schema.GroupResource{Group: machineAPIGroup, Resource: "MachineSet"}, name)
	if err != nil {
		return fmt.Errorf("error calling scaleClient.Scales get: %v", err)
	}

	scaleUpdate := scale.DeepCopy()
	scaleUpdate.Spec.Replicas = int32(replicas)
	_, err = scaleClient.Scales(e2e.TestContext.MachineApiNamespace).Update(schema.GroupResource{Group: machineAPIGroup, Resource: "MachineSet"}, scaleUpdate)
	if err != nil {
		return fmt.Errorf("error calling scaleClient.Scales update: %v", err)
	}
	return nil
}

// getScaleClient returns a ScalesGetter object to manipulate scale subresources
func getScaleClient() (scale.ScalesGetter, error) {
	cfg, err := e2e.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("error getting config %v", err)
	}
	mapper, err := apiutil.NewDiscoveryRESTMapper(cfg)
	if err != nil {
		return nil, fmt.Errorf("error calling NewDiscoveryRESTMapper %v", err)
	}

	discovery := discovery.NewDiscoveryClientForConfigOrDie(cfg)
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(discovery)
	scaleClient, err := scale.NewForConfig(cfg, mapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)
	if err != nil {
		return nil, fmt.Errorf("error calling building scale client %v", err)
	}
	return scaleClient, nil
}

// nodesSnapShotLogs logs the state of all the nodes in the cluster
func nodesSnapShotLogs(client runtimeclient.Client) error {
	nodes, err := e2e.GetNodes(client)
	if err != nil {
		return fmt.Errorf("error getting nodes: %v", err)
	}

	for key, node := range nodes {
		glog.Infof("Node %q. Ready: %t. Unschedulable: %t", node.Name, e2e.IsNodeReady(&nodes[key]), node.Spec.Unschedulable)
	}
	return nil
}

func waitForClusterSizeToBeHealthy(client runtimeclient.Client, targetSize int) error {
	if err := wait.PollImmediate(5*time.Second, e2e.WaitLong, func() (bool, error) {
		glog.Infof("Cluster size expected to be %d nodes", targetSize)
		if err := machineSetsSnapShotLogs(client); err != nil {
			return false, err
		}

		if err := nodesSnapShotLogs(client); err != nil {
			return false, err
		}

		finalClusterSize, err := getClusterSize(client)
		if err != nil {
			return false, err
		}
		return finalClusterSize == targetSize, nil
	}); err != nil {
		return fmt.Errorf("Did not reach expected number of nodes: %v", err)
	}

	glog.Infof("waiting for all nodes to be ready")
	if err := e2e.WaitUntilAllNodesAreReady(client); err != nil {
		return err
	}

	glog.Infof("waiting for all nodes to be schedulable")
	if err := waitUntilAllNodesAreSchedulable(client); err != nil {
		return err
	}

	glog.Infof("waiting for each node to be backed by a machine")
	if !isOneMachinePerNode(client) {
		return fmt.Errorf("One machine per node condition violated")
	}

	return nil
}

// waitUntilAllNodesAreSchedulable waits for all cluster nodes to be schedulable and returns an error otherwise
func waitUntilAllNodesAreSchedulable(client runtimeclient.Client) error {
	return wait.PollImmediate(1*time.Second, time.Minute, func() (bool, error) {
		nodeList := corev1.NodeList{}
		if err := client.List(context.TODO(), &runtimeclient.ListOptions{}, &nodeList); err != nil {
			glog.Errorf("error querying api for nodeList object: %v, retrying...", err)
			return false, nil
		}
		// All nodes needs to be schedulable
		for _, node := range nodeList.Items {
			if node.Spec.Unschedulable == true {
				glog.Errorf("Node %q is unschedulable", node.Name)
				return false, nil
			}
			glog.Infof("Node %q is schedulable", node.Name)
		}
		return true, nil
	})
}
